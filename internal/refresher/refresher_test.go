package refresher_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/wasm"
	"github.com/soroworks/sorovault/internal/wasm/wasmtest"
)

const interval = time.Hour

// clock is a settable time source shared by the store and the refresher, so
// "stale" is decided by the test rather than the wall clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// contractID derives a distinct, valid contract strkey from n.
func contractID(t *testing.T, n int) string {
	t.Helper()
	var raw [32]byte
	raw[0], raw[1] = byte(n), byte(n>>8)
	id, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	require.NoError(t, err)
	return id
}

func moduleWith(t *testing.T, fnName string) []byte {
	t.Helper()
	entry := xdr.ScSpecEntry{
		Kind: xdr.ScSpecEntryKindScSpecEntryFunctionV0,
		FunctionV0: &xdr.ScSpecFunctionV0{
			Name:    xdr.ScSymbol(fnName),
			Inputs:  []xdr.ScSpecFunctionInputV0{},
			Outputs: []xdr.ScSpecTypeDef{},
		},
	}
	body, err := entry.MarshalBinary()
	require.NoError(t, err)
	return wasmtest.Module(wasmtest.Section{Name: wasm.SectionContractSpec, Body: body})
}

type fixture struct {
	reg   *registry.Registry
	fake  *stellartest.Fake
	db    *store.Memory
	clock *clock
	log   *slog.Logger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	db := store.NewMemory()
	db.SetClock(c.Now)
	fake := stellartest.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := registry.New(fake, spec.XDRDecoder{}, db, registry.Options{Logger: log})
	return &fixture{reg: reg, fake: fake, db: db, clock: c, log: log}
}

func (f *fixture) refresher(t *testing.T) *refresher.Refresher {
	t.Helper()
	r, err := refresher.New(f.reg, refresher.Options{
		Interval: interval,
		Logger:   f.log,
		Now:      f.clock.Now,
		Wait: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.clock.Advance(d)
			return nil
		},
	})
	require.NoError(t, err)
	return r
}

func TestSweepSpreadsRefreshesAcrossInterval(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		f.register(t, contractID(t, i), "hello")
	}
	f.clock.Advance(interval + time.Minute)

	var waits []time.Duration
	r, err := refresher.New(f.reg, refresher.Options{
		Interval: interval,
		Logger:   f.log,
		Now:      f.clock.Now,
		Wait: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			f.clock.Advance(d)
			return ctx.Err()
		},
	})
	require.NoError(t, err)

	sum, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, sum.Checked)
	assert.Equal(t, []time.Duration{interval / 4, interval / 4, interval / 4}, waits,
		"refreshes after the first must start at evenly spaced offsets")
}

func (f *fixture) register(t *testing.T, id, fn string) {
	t.Helper()
	f.fake.Deploy(id, moduleWith(t, fn))
	_, err := f.reg.Register(context.Background(), id)
	require.NoError(t, err)
}

func TestNewRejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, -time.Second} {
		_, err := refresher.New(newFixture(t).reg, refresher.Options{Interval: d})
		assert.Error(t, err, "interval %s", d)
	}
}

func TestSweepSkipsRecentlyRefreshed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, contractID(t, 1), "hello")

	f.clock.Advance(interval / 2)
	before := f.fake.Calls["WasmHash"]

	sum, err := f.refresher(t).Sweep(context.Background())
	require.NoError(t, err)

	assert.Equal(t, refresher.Summary{}, sum)
	assert.Equal(t, before, f.fake.Calls["WasmHash"], "a fresh contract must not cost an RPC call")
}

func TestSweepDetectsUpgrade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "hello")

	f.fake.Upgrade(id, moduleWith(t, "goodbye"))
	f.clock.Advance(interval + time.Minute)

	sum, err := f.refresher(t).Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{Checked: 1, Changed: 1}, sum)

	got, err := f.db.GetSpec(context.Background(), "testnet", id, "")
	require.NoError(t, err)
	require.Len(t, got.Interface.Functions, 1)
	assert.Equal(t, "goodbye", got.Interface.Functions[0].Name)

	versions, err := f.db.ListVersions(context.Background(), "testnet", id)
	require.NoError(t, err)
	assert.Len(t, versions, 2, "the superseded interface must be kept")
}

func TestSweepAdvancesLastRefreshedOnUnchanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "hello")

	f.clock.Advance(interval + time.Minute)
	r := f.refresher(t)
	fetches := f.fake.Calls["FetchContract"]

	sum, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{Checked: 1}, sum)
	assert.Equal(t, fetches, f.fake.Calls["FetchContract"], "an unchanged contract must not be re-downloaded")

	// It was just confirmed, so an immediate second sweep has nothing to do.
	sum, err = r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{}, sum)
}

func TestSweepContinuesPastFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	gone, kept := contractID(t, 1), contractID(t, 2)
	f.register(t, gone, "hello")
	f.register(t, kept, "hello")

	f.fake.Remove(gone)
	f.fake.Upgrade(kept, moduleWith(t, "v2"))
	f.clock.Advance(interval + time.Minute)

	sum, err := f.refresher(t).Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{Checked: 2, Changed: 1, Failed: 1}, sum)
}

// More contracts than fit on one listing page, every one of which moves to
// the front of the listing as it is refreshed. Each must be checked once.
func TestSweepVisitsEveryContractAcrossPages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const n = store.MaxLimit + 25
	for i := 0; i < n; i++ {
		f.register(t, contractID(t, i), "hello")
		f.clock.Advance(time.Second)
	}
	f.clock.Advance(interval)
	before := f.fake.Calls["WasmHash"]

	sum, err := f.refresher(t).Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, n, sum.Checked)
	assert.Equal(t, n, f.fake.Calls["WasmHash"]-before)
}

func TestSweepFailsWhenNetworkUnresolvable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fake.NetworkErr = errors.New("rpc down")

	_, err := f.refresher(t).Sweep(context.Background())
	assert.ErrorContains(t, err, "rpc down")
}

// TestSweepDefersWhenTheRegistryIsBusy proves a busy registry defers a
// refresh rather than failing it: the contract stays stale, so the next
// sweep picks it up.
func TestSweepDefersWhenTheRegistryIsBusy(t *testing.T) {
	t.Parallel()

	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	db := store.NewMemory()
	db.SetClock(c.Now)
	fake := stellartest.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := registry.New(fake, spec.XDRDecoder{}, db, registry.Options{
		Logger:         log,
		MaxConcurrent:  1,
		AcquireTimeout: 50 * time.Millisecond,
	})

	id := contractID(t, 1)
	fake.Deploy(id, moduleWith(t, "hello"))
	_, err := reg.Register(context.Background(), id)
	require.NoError(t, err)

	// Hold the only slot with a registration that does not finish.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fake.BeforeFetch = func(ctx context.Context, contractID string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	held := make(chan struct{})
	go func() {
		defer close(held)
		_, _ = reg.Register(context.Background(), id)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to start")
	}

	c.Advance(interval + time.Minute)
	r, err := refresher.New(reg, refresher.Options{Interval: interval, Logger: log, Now: c.Now})
	require.NoError(t, err)

	sum, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{Checked: 1, Busy: 1}, sum,
		"a busy registry defers the refresh rather than failing it")

	close(release)
	<-held
}

func TestRunStopsOnCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, contractID(t, 1), "hello")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.refresher(t).Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

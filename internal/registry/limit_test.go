package registry_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
)

// acquireTimeout is how long a test lets an operation wait for a slot
// before it is refused. It is short enough to keep the tests quick and
// long enough to be distinguishable from a scheduling hiccup.
const acquireTimeout = 100 * time.Millisecond

// blockingNetwork is a fake network whose fetches wait to be released, so
// that a test can hold registrations open and watch what the registry does
// with the ones that do not get a slot.
type blockingNetwork struct {
	*stellartest.Fake

	// entered counts the fetches that have started.
	entered chan struct{}
	// release is closed to let every held fetch finish.
	release chan struct{}
	// inFlight and peak track how many fetches run at once.
	inFlight atomic.Int64
	peak     atomic.Int64
}

// newBlockingNetwork returns a fake network whose fetches block until
// release is closed.
func newBlockingNetwork(t *testing.T) *blockingNetwork {
	t.Helper()

	b := &blockingNetwork{
		Fake:    stellartest.New(),
		entered: make(chan struct{}, 64),
		release: make(chan struct{}),
	}
	b.Fake.BeforeFetch = func(ctx context.Context, contractID string) error {
		inFlight := b.inFlight.Add(1)
		for {
			peak := b.peak.Load()
			if inFlight <= peak || b.peak.CompareAndSwap(peak, inFlight) {
				break
			}
		}
		defer b.inFlight.Add(-1)

		select {
		case b.entered <- struct{}{}:
		default:
		}

		select {
		case <-b.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b
}

// releaseAll lets every held fetch finish.
func (b *blockingNetwork) releaseAll() { close(b.release) }

// registry over the blocking network, with the given concurrency limit.
func (b *blockingNetwork) registry(t *testing.T, limit int) *registry.Registry {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return registry.New(b.Fake, spec.XDRDecoder{}, store.NewMemory(), registry.Options{
		Logger:         log,
		MaxConcurrent:  limit,
		AcquireTimeout: acquireTimeout,
	})
}

// TestRegisterRunsAtMostLimitAtOnce proves the bound itself: however many
// clients ask, no more than the limit register at the same time.
func TestRegisterRunsAtMostLimitAtOnce(t *testing.T) {
	t.Parallel()

	const limit = 2

	network := newBlockingNetwork(t)
	network.Deploy(idA, moduleWith(t, "hello"))
	reg := network.registry(t, limit)

	// Ask for more registrations than the limit allows.
	const extra = 3
	results := make(chan error, limit+extra)
	for range limit + extra {
		go func() {
			_, err := reg.Register(context.Background(), idA)
			results <- err
		}()
	}

	// The limit's worth of fetches start; no more than that.
	for range limit {
		select {
		case <-network.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a registration to start")
		}
	}
	select {
	case <-network.entered:
		t.Fatal("more registrations ran at once than the limit allows")
	case <-time.After(2 * acquireTimeout):
	}

	// The ones that did not get a slot are refused rather than queued.
	for range extra {
		select {
		case err := <-results:
			require.ErrorIs(t, err, registry.ErrBusy,
				"a registration that cannot get a slot must be refused")
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a registration to be refused")
		}
	}

	assert.Equal(t, int64(limit), network.peak.Load(),
		"the registry must not run more registrations at once than its limit")

	// Releasing the held fetches lets them finish.
	network.releaseAll()
	for range limit {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a registration to finish")
		}
	}
}

// TestRegisterReleasesCapacityAfterFailure proves a failed registration
// gives its slot back, so one bad contract cannot wedge the registry.
func TestRegisterReleasesCapacityAfterFailure(t *testing.T) {
	t.Parallel()

	network := newBlockingNetwork(t)
	network.Deploy(idA, moduleWith(t, "hello"))
	network.Fake.FetchErr = errors.New("the network is having a bad day")
	reg := network.registry(t, 1)

	done := make(chan error, 1)
	go func() {
		_, err := reg.Register(context.Background(), idA)
		done <- err
	}()

	// Let the registration reach the fetch, then fail.
	select {
	case <-network.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to start")
	}
	network.releaseAll()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad day")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to fail")
	}

	// The slot is free again: a second registration is not refused for
	// lack of capacity.
	network.Fake.FetchErr = nil
	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err, "a failed registration must not hold a slot")
}

// TestRegisterReleasesCapacityAfterCancellation proves a cancelled
// registration gives its slot back.
func TestRegisterReleasesCapacityAfterCancellation(t *testing.T) {
	t.Parallel()

	network := newBlockingNetwork(t)
	network.Deploy(idA, moduleWith(t, "hello"))
	reg := network.registry(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := reg.Register(ctx, idA)
		done <- err
	}()

	// Wait until the registration is inside the fetch, then cancel it.
	select {
	case <-network.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to start")
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to be cancelled")
	}

	// The slot is free again.
	network.releaseAll()
	_, err := reg.Register(context.Background(), idA)
	require.NoError(t, err, "a cancelled registration must not hold a slot")
}

// TestRefreshSharesTheRegistrationBudget proves a refresh draws on the same
// budget as a registration: the refresher gets no more of the registry's
// capacity than an API client does.
func TestRefreshSharesTheRegistrationBudget(t *testing.T) {
	t.Parallel()

	network := newBlockingNetwork(t)
	network.Deploy(idA, moduleWith(t, "hello"))
	reg := network.registry(t, 1)

	// A registration takes the only slot and stays in it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = reg.Register(context.Background(), idA)
	}()

	select {
	case <-network.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to start")
	}

	// A refresh wants the same slot, and is refused.
	_, err := reg.Refresh(context.Background(), idA)
	require.ErrorIs(t, err, registry.ErrBusy,
		"a refresh must not get capacity a registration is using")

	network.releaseAll()
	<-done
}

// TestRegistryReportsItsLimit proves the registry says what it is doing,
// so the API can tell a client how long to wait.
func TestRegistryReportsItsLimit(t *testing.T) {
	t.Parallel()

	reg, _, _ := newRegistry(t)
	assert.Equal(t, registry.DefaultMaxConcurrent, reg.MaxConcurrent())
	assert.Equal(t, registry.DefaultAcquireTimeout, reg.AcquireTimeout())
}

// TestCheckPassphrase proves the network guard can be asked directly, which
// is what the doctor command does.
func TestCheckPassphrase(t *testing.T) {
	t.Parallel()

	assert.NoError(t, registry.CheckPassphrase(stellar.NetworkTestnet, stellar.NetworkTestnet))
	assert.NoError(t, registry.CheckPassphrase(stellar.NetworkTestnet, ""),
		"an empty expectation accepts whatever the endpoint serves")

	err := registry.CheckPassphrase(stellar.NetworkPublic, stellar.NetworkTestnet)
	require.ErrorIs(t, err, registry.ErrNetworkMismatch)
	assert.Contains(t, err.Error(), stellar.NetworkPublic)
}

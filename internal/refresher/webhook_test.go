package refresher_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/refresher"
)

// recorded is one delivery the test server received.
type recorded struct {
	body      []byte
	signature string
	content   string
}

// recorder is an HTTP server that keeps what it was sent.
type recorder struct {
	mu       sync.Mutex
	requests []recorded
	status   int
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, recorded{
		body:      body,
		signature: req.Header.Get(refresher.SignatureHeader),
		content:   req.Header.Get("Content-Type"),
	})
	if r.status != 0 {
		w.WriteHeader(r.status)
	}
}

// all returns every request the server received.
func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.requests...)
}

// newRecorder starts a test server and returns it with its address.
func newRecorder(t *testing.T) (*recorder, string) {
	t.Helper()

	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return rec, srv.URL
}

// TestSweepNotifiesTheWebhook proves a contract that changed during a sweep
// is reported to the webhook.
func TestSweepNotifiesTheWebhook(t *testing.T) {
	t.Parallel()

	rec, url := newRecorder(t)
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "v1")

	f.fake.Upgrade(id, moduleWith(t, "v2"))
	f.clock.Advance(interval + time.Minute)

	r := f.refresherWith(t, refresher.WebhookOptions{URL: url})
	sum, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{Checked: 1, Changed: 1}, sum)

	requests := rec.all()
	require.Len(t, requests, 1, "one POST per contract that changed")

	var change refresher.Change
	require.NoError(t, json.Unmarshal(requests[0].body, &change))
	assert.Equal(t, id, change.ContractID)
	assert.Equal(t, "testnet", change.Network)
	assert.NotEmpty(t, change.OldWasmHash)
	assert.NotEmpty(t, change.NewWasmHash)
	assert.NotEqual(t, change.OldWasmHash, change.NewWasmHash)
	assert.Contains(t, change.InterfaceURL, id)
	assert.Contains(t, change.InterfaceURL, "network=testnet")
	assert.False(t, change.ChangedAt.IsZero())
	assert.Equal(t, "application/json", requests[0].content)
}

// TestWebhookSignsThePayload proves the payload is signed when a secret is
// configured, and left alone when it is not.
func TestWebhookSignsThePayload(t *testing.T) {
	t.Parallel()

	rec, url := newRecorder(t)
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "v1")

	f.fake.Upgrade(id, moduleWith(t, "v2"))
	f.clock.Advance(interval + time.Minute)

	const secret = "shared-secret"
	r := f.refresherWith(t, refresher.WebhookOptions{URL: url, Secret: secret})
	_, err := r.Sweep(context.Background())
	require.NoError(t, err)

	requests := rec.all()
	require.Len(t, requests, 1)
	assert.Equal(t, refresher.Sign(secret, requests[0].body), requests[0].signature,
		"the receiver can recompute the signature over the raw body")
}

// TestWebhookIsSilentWithoutASecret proves an unsigned deployment sends no
// signature header.
func TestWebhookIsSilentWithoutASecret(t *testing.T) {
	t.Parallel()

	rec, url := newRecorder(t)
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "v1")

	f.fake.Upgrade(id, moduleWith(t, "v2"))
	f.clock.Advance(interval + time.Minute)

	r := f.refresherWith(t, refresher.WebhookOptions{URL: url})
	_, err := r.Sweep(context.Background())
	require.NoError(t, err)

	requests := rec.all()
	require.Len(t, requests, 1)
	assert.Empty(t, requests[0].signature)
}

// TestWebhookFailureDoesNotStopTheSweep proves a receiver that is down
// costs the sweep nothing: the refresh itself still happened.
func TestWebhookFailureDoesNotStopTheSweep(t *testing.T) {
	t.Parallel()

	rec, url := newRecorder(t)
	rec.status = http.StatusInternalServerError
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "v1")

	f.fake.Upgrade(id, moduleWith(t, "v2"))
	f.clock.Advance(interval + time.Minute)

	r := f.refresherWith(t, refresher.WebhookOptions{URL: url})
	sum, err := r.Sweep(context.Background())
	require.NoError(t, err, "a failed delivery must not fail the sweep")
	assert.Equal(t, refresher.Summary{Checked: 1, Changed: 1}, sum)

	// The upgrade was recorded even though the webhook refused it.
	got, err := f.db.GetSpec(context.Background(), "testnet", id, "")
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Interface.Functions[0].Name)
}

// TestWebhookIsNotCalledForUnchangedContracts proves the webhook only hears
// about interfaces that actually changed.
func TestWebhookIsNotCalledForUnchangedContracts(t *testing.T) {
	t.Parallel()

	rec, url := newRecorder(t)
	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "v1")

	f.clock.Advance(interval + time.Minute)
	r := f.refresherWith(t, refresher.WebhookOptions{URL: url})
	_, err := r.Sweep(context.Background())
	require.NoError(t, err)

	assert.Empty(t, rec.all())
}

// TestWebhookTimesOut proves a receiver that hangs cannot stall the sweep:
// the delivery is abandoned rather than retried.
func TestWebhookTimesOut(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	f := newFixture(t)
	id := contractID(t, 1)
	f.register(t, id, "v1")

	f.fake.Upgrade(id, moduleWith(t, "v2"))
	f.clock.Advance(interval + time.Minute)

	r := f.refresherWith(t, refresher.WebhookOptions{
		URL:     srv.URL,
		Timeout: 50 * time.Millisecond,
	})
	sum, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refresher.Summary{Checked: 1, Changed: 1}, sum)
}

func TestNewWebhookRejectsABadURL(t *testing.T) {
	t.Parallel()

	for _, url := range []string{"", "not a url", "/relative", "ftp://example.test/hook"} {
		_, err := refresher.NewWebhook(refresher.WebhookOptions{URL: url})
		assert.Error(t, err, "url %q", url)
	}
}

// TestWebhookPostsTheChange proves the webhook client posts what it is
// told, and reports a refusal as an error.
func TestWebhookPostsTheChange(t *testing.T) {
	t.Parallel()

	rec, url := newRecorder(t)
	webhook, err := refresher.NewWebhook(refresher.WebhookOptions{
		URL:    url,
		Secret: "s3cret",
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)

	change := refresher.Change{
		ContractID:   "CDZZXVGUCLBUTE3WDD6TF2NXPTBPOYTVPPBBIZMSPVIQLUG226XJ4PAN",
		Network:      "testnet",
		OldWasmHash:  "old",
		NewWasmHash:  "new",
		InterfaceURL: "/api/contracts/x",
	}
	require.NoError(t, webhook.Notify(context.Background(), change))

	requests := rec.all()
	require.Len(t, requests, 1)
	assert.True(t, strings.Contains(requests[0].signature, "sha256="))
	assert.Equal(t, refresher.Sign("s3cret", requests[0].body), requests[0].signature)

	var got refresher.Change
	require.NoError(t, json.Unmarshal(requests[0].body, &got))
	assert.Equal(t, change.ContractID, got.ContractID)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), got.ChangedAt.UTC())

	rec.status = http.StatusBadGateway
	assert.Error(t, webhook.Notify(context.Background(), change))
}

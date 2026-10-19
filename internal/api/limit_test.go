package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/registry"
)

// busyAcquireTimeout is how long a request waits for a slot before it is
// told to come back. It is short enough to keep the test quick.
const busyAcquireTimeout = 100 * time.Millisecond

// blockingHarness is a harness whose fetches wait to be released, so that
// one request can be made to hold the registry's only slot.
type blockingHarness struct {
	*harness

	entered chan struct{}
	release chan struct{}
}

// newBlockingHarness builds a harness with a concurrency limit of one and a
// network that holds its fetches open.
func newBlockingHarness(t *testing.T) *blockingHarness {
	t.Helper()

	h := &blockingHarness{
		harness: newHarnessWith(t, registry.Options{
			MaxConcurrent:  1,
			AcquireTimeout: busyAcquireTimeout,
		}),
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}

	h.fake.BeforeFetch = func(ctx context.Context, contractID string) error {
		select {
		case h.entered <- struct{}{}:
		default:
		}
		select {
		case <-h.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return h
}

// startRegistration begins a registration that will hold the only slot, and
// returns a function that waits for it to finish.
func (h *blockingHarness) startRegistration(t *testing.T, contractID string) func() {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := h.server.Client().Post(h.server.URL+"/api/contracts",
			"application/json", strings.NewReader(`{"contract_id":"`+contractID+`"}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the registration to start")
	}

	return func() {
		close(h.release)
		<-done
	}
}

// TestRegisterOverCapacityReturns503 proves that a client which cannot be
// given a slot is told to come back rather than left waiting.
func TestRegisterOverCapacityReturns503(t *testing.T) {
	t.Parallel()

	h := newBlockingHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	finish := h.startRegistration(t, idA)
	defer finish()

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	assert.Equal(t, "1", resp.Header.Get("Retry-After"),
		"a refused request must say how long to wait")

	body := h.decode(t, raw)
	assert.Contains(t, body["error"], "too many registrations")
}

// TestRefreshOverCapacityReturns503 proves the same for a refresh.
func TestRefreshOverCapacityReturns503(t *testing.T) {
	t.Parallel()

	h := newBlockingHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	finish := h.startRegistration(t, idA)
	defer finish()

	resp, raw := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))
}

// TestRegisterSucceedsOnceCapacityIsFree proves the registry recovers: a
// request that is refused does not poison the ones after it.
func TestRegisterSucceedsOnceCapacityIsFree(t *testing.T) {
	t.Parallel()

	h := newBlockingHarness(t)
	h.fake.Deploy(idA, moduleWith(t, "balance"))

	finish := h.startRegistration(t, idA)

	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))

	finish()

	// The slot is free again, so the next registration is served. The
	// contract is already known by then, so it is re-verified rather than
	// created.
	resp, raw = h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.False(t, body["created"].(bool))
}

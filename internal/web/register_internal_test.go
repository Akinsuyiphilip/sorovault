package web

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/wasm"
)

// TestRegisterStatus checks the form's failures carry the same meaning as the
// JSON API's status codes, including the busy registry the concurrency budget
// produces.
func TestRegisterStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"not found", fmt.Errorf("%w: CDZZ…", store.ErrNotFound), http.StatusNotFound},
		{"contract missing", fmt.Errorf("%w: CDZZ…", stellar.ErrContractNotFound), http.StatusNotFound},
		{"no wasm", stellar.ErrNoWasm, http.StatusUnprocessableEntity},
		{"no spec section", spec.ErrNoSpecSection, http.StatusUnprocessableEntity},
		{"decode limit", spec.ErrLimitExceeded, http.StatusUnprocessableEntity},
		{"module too large", wasm.ErrModuleTooLarge, http.StatusUnprocessableEntity},
		{"network mismatch", registry.ErrNetworkMismatch, http.StatusServiceUnavailable},
		{"busy", fmt.Errorf("%w: 4 already running", registry.ErrBusy), http.StatusServiceUnavailable},
		{"invalid contract id", errors.New(`"nope" is not a valid contract address`), http.StatusBadRequest},
		{"unknown", errors.New("connection refused"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, message := registerStatus(tt.err)
			assert.Equal(t, tt.status, status)
			assert.NotEmpty(t, message)
			if tt.status == http.StatusInternalServerError {
				assert.NotContains(t, message, tt.err.Error(), "an internal failure must not leak")
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wait time.Duration
		want int
	}{
		{0, 1},
		{200 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{2 * time.Second, 2},
		{2100 * time.Millisecond, 3},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, retryAfter(tt.wait), tt.wait.String())
	}
}

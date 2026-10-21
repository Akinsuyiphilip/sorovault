package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
)

// TestStatsReportsRegistrySize covers the counts the statistics endpoint
// exists for: contracts per network, stored versions, and the newest
// registration.
func TestStatsReportsRegistrySize(t *testing.T) {
	t.Parallel()

	memory := store.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	memory.SetClock(func() time.Time {
		now = now.Add(time.Minute)
		return now
	})
	h := newHarnessWithStore(t, stellartest.New(), memory)

	t.Run("empty registry", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/stats", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, []any{}, got["networks"])
		assert.Equal(t, float64(0), got["contracts"])
		assert.Equal(t, float64(0), got["versions"])
		assert.NotContains(t, got, "latest_contract")
		assert.NotContains(t, got, "last_refresh", "refresh is off, so no sweep is reported")
	})

	h.fake.Deploy(idA, moduleWith(t, "v1"))
	h.fake.Deploy(idB, moduleWith(t, "balance"))
	for _, id := range []string{idA, idB} {
		resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+id+`"}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	}

	t.Run("counts contracts and versions", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/stats", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["contracts"])
		assert.Equal(t, float64(2), got["versions"])

		networks := got["networks"].([]any)
		require.Len(t, networks, 1)
		network := networks[0].(map[string]any)
		assert.Equal(t, "testnet", network["network"])
		assert.Equal(t, float64(2), network["contracts"])
		assert.Equal(t, float64(2), network["versions"])

		latest := got["latest_contract"].(map[string]any)
		assert.Equal(t, idB, latest["contract_id"],
			"the most recently registered contract is the newest first_seen")
	})

	t.Run("an upgrade adds a version but not a contract", func(t *testing.T) {
		h.fake.Upgrade(idA, moduleWith(t, "v2"))
		resp, raw := h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		resp, raw = h.do(t, http.MethodGet, "/api/stats", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["contracts"])
		assert.Equal(t, float64(3), got["versions"],
			"the superseded interface is still stored")
		assert.Equal(t, idB, got["latest_contract"].(map[string]any)["contract_id"],
			"refreshing a contract does not make it newly registered")
	})
}

// TestStatsReportsTheLastRefreshSweep proves the in-memory sweep summary is
// exposed when a refresher is wired in, and omitted when there has not been
// a completed sweep yet.
func TestStatsReportsTheLastRefreshSweep(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	h.api.SetLastSweep(func() (refresher.SweepRecord, bool) { return refresher.SweepRecord{}, false })
	resp, raw := h.do(t, http.MethodGet, "/api/stats", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.NotContains(t, h.decode(t, raw), "last_refresh",
		"a sweep that has not finished yet must not be reported as one that did")

	completed := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	h.api.SetLastSweep(func() (refresher.SweepRecord, bool) {
		return refresher.SweepRecord{
			Summary: refresher.Summary{
				Checked: 9,
				Changed: 2,
				Failed:  1,
				Busy:    1,
				Missing: 3,
			},
			CompletedAt: completed,
		}, true
	})

	resp, raw = h.do(t, http.MethodGet, "/api/stats", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	last := h.decode(t, raw)["last_refresh"].(map[string]any)
	assert.Equal(t, float64(9), last["checked"])
	assert.Equal(t, float64(2), last["changed"])
	assert.Equal(t, float64(1), last["failed"])
	assert.Equal(t, float64(1), last["busy"])
	assert.Equal(t, float64(3), last["missing"])
	assert.Equal(t, completed.Format(time.RFC3339), last["completed_at"])
}

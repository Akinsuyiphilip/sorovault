package api_test

import (
	"encoding/xml"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/stellar/stellartest"
	"github.com/soroworks/sorovault/internal/store"
)

// upgradeFixture registers one contract and upgrades it twice, with a clock
// that advances on every store write so the events order deterministically.
func upgradeFixture(t *testing.T) (*harness, []string) {
	t.Helper()

	memory := store.NewMemory()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	memory.SetClock(func() time.Time {
		now = now.Add(time.Minute)
		return now
	})
	h := newHarnessWithStore(t, stellartest.New(), memory)

	v1 := moduleWith(t, "v1")
	h.fake.Deploy(idA, v1)
	resp, raw := h.do(t, http.MethodPost, "/api/contracts", `{"contract_id":"`+idA+`"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))

	v2 := moduleWith(t, "v2")
	h.fake.Upgrade(idA, v2)
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	v3 := moduleWith(t, "v3")
	h.fake.Upgrade(idA, v3)
	resp, raw = h.do(t, http.MethodPost, "/api/contracts/"+idA+"/refresh", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	return h, []string{stellartest.HashOf(v1), stellartest.HashOf(v2), stellartest.HashOf(v3)}
}

func TestUpgradesFeed(t *testing.T) {
	t.Parallel()

	h, hashes := upgradeFixture(t)

	t.Run("first registration is not an upgrade", func(t *testing.T) {
		// The fixture has two upgrades, not three: the initial version is
		// how the contract entered the registry.
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["total"])
		assert.Len(t, got["upgrades"].([]any), 2)
	})

	t.Run("newest first with both hashes", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		upgrades := h.decode(t, raw)["upgrades"].([]any)
		require.Len(t, upgrades, 2)

		newest := upgrades[0].(map[string]any)
		assert.Equal(t, idA, newest["contract_id"])
		assert.Equal(t, "testnet", newest["network"])
		assert.Equal(t, hashes[1], newest["old_wasm_hash"])
		assert.Equal(t, hashes[2], newest["new_wasm_hash"])
		assert.NotEmpty(t, newest["observed_at"])

		older := upgrades[1].(map[string]any)
		assert.Equal(t, hashes[0], older["old_wasm_hash"])
		assert.Equal(t, hashes[1], older["new_wasm_hash"])
	})

	t.Run("paging", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades?limit=1&offset=1", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(2), got["total"], "total ignores paging")
		assert.Equal(t, float64(1), got["limit"])
		assert.Equal(t, float64(1), got["offset"])

		upgrades := got["upgrades"].([]any)
		require.Len(t, upgrades, 1)
		assert.Equal(t, hashes[1], upgrades[0].(map[string]any)["new_wasm_hash"],
			"the second page holds the older upgrade")
	})

	t.Run("past the end still reports the total", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades?offset=99", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, []any{}, got["upgrades"])
		assert.Equal(t, float64(2), got["total"])
	})

	t.Run("network filter", func(t *testing.T) {
		resp, raw := h.do(t, http.MethodGet, "/api/upgrades?network=public", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		got := h.decode(t, raw)
		assert.Equal(t, float64(0), got["total"])
		assert.Equal(t, []any{}, got["upgrades"])
	})
}

// TestUpgradesAtomFeed proves the Atom form carries the same events with the
// fields a feed reader needs, and that paging is linked.
func TestUpgradesAtomFeed(t *testing.T) {
	t.Parallel()

	h, hashes := upgradeFixture(t)

	type atomDocument struct {
		ID      string `xml:"id"`
		Title   string `xml:"title"`
		Updated string `xml:"updated"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
		Entries []struct {
			ID      string `xml:"id"`
			Title   string `xml:"title"`
			Updated string `xml:"updated"`
			Summary string `xml:"summary"`
			Link    struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}

	resp, raw := h.do(t, http.MethodGet, "/api/upgrades.atom", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/atom+xml")

	var feed atomDocument
	require.NoError(t, xml.Unmarshal(raw, &feed), "the feed must be well-formed Atom XML")

	assert.Equal(t, "urn:sorovault:upgrades", feed.ID)
	assert.NotEmpty(t, feed.Updated)
	require.Len(t, feed.Entries, 2)

	newest := feed.Entries[0]
	assert.Contains(t, newest.Title, idA)
	assert.Contains(t, newest.Summary, hashes[1])
	assert.Contains(t, newest.Summary, hashes[2])
	assert.NotEmpty(t, newest.Updated)
	_, err := time.Parse(time.RFC3339, newest.Updated)
	assert.NoError(t, err, "Atom requires an RFC 3339 timestamp")
	assert.Contains(t, newest.Link.Href, "/api/contracts/"+idA)

	assert.Contains(t, feed.Entries[1].Summary, hashes[0],
		"entries are newest first, like the JSON feed")

	resp, raw = h.do(t, http.MethodGet, "/api/upgrades.atom?limit=1", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	feed = atomDocument{}
	require.NoError(t, xml.Unmarshal(raw, &feed))
	require.Len(t, feed.Entries, 1)

	var next string
	for _, link := range feed.Links {
		if link.Rel == "next" {
			next = link.Href
		}
	}
	assert.Contains(t, next, "offset=1", "a page with more events links to the next one")
}

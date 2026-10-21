package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/soroworks/sorovault/internal/store"
)

// atomNamespace is the Atom 1.0 namespace (RFC 4287).
const atomNamespace = "http://www.w3.org/2005/Atom"

// atomFeed is the document served at /api/upgrades.atom.
type atomFeed struct {
	XMLName  xml.Name    `xml:"feed"`
	XMLNS    string      `xml:"xmlns,attr"`
	ID       string      `xml:"id"`
	Title    string      `xml:"title"`
	Subtitle string      `xml:"subtitle,omitempty"`
	Updated  string      `xml:"updated"`
	Author   atomAuthor  `xml:"author"`
	Links    []atomLink  `xml:"link"`
	Entries  []atomEntry `xml:"entry"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr,omitempty"`
}

type atomEntry struct {
	ID      string   `xml:"id"`
	Title   string   `xml:"title"`
	Updated string   `xml:"updated"`
	Link    atomLink `xml:"link"`
	Summary string   `xml:"summary"`
}

// upgradesAtom serves GET /api/upgrades.atom: the same upgrades as the JSON
// feed, as an Atom 1.0 document.
func (s *Server) upgradesAtom(w http.ResponseWriter, r *http.Request) {
	filter := upgradeFilter(r)

	page, err := s.reg.Store().ListUpgrades(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	feed := atomFeed{
		XMLNS:    atomNamespace,
		ID:       "urn:sorovault:upgrades",
		Title:    "SoroVault interface upgrades",
		Subtitle: "Contracts whose interface changed, newest first",
		Updated:  feedUpdated(page.Upgrades),
		Author:   atomAuthor{Name: "SoroVault"},
		Links: []atomLink{{
			Rel:  "self",
			Href: atomURL(filter),
			Type: "application/atom+xml",
		}},
		Entries: []atomEntry{},
	}

	if next, ok := nextUpgradePage(filter, page); ok {
		feed.Links = append(feed.Links, atomLink{
			Rel:  "next",
			Href: atomURL(next),
			Type: "application/atom+xml",
		})
	}

	for _, upgrade := range page.Upgrades {
		feed.Entries = append(feed.Entries, atomEntry{
			ID:    fmt.Sprintf("urn:sorovault:upgrade:%s:%s:%s", upgrade.Network, upgrade.ContractID, upgrade.NewWasmHash),
			Title: fmt.Sprintf("%s upgraded on %s", upgrade.ContractID, upgrade.Network),
			// RFC 4287 requires an RFC 3339 timestamp.
			Updated: upgrade.ObservedAt.UTC().Format(time.RFC3339),
			Link: atomLink{
				Rel:  "alternate",
				Href: contractURL(upgrade),
				Type: "application/json",
			},
			Summary: fmt.Sprintf(
				"Contract %s on %s moved from WASM hash %s to %s.",
				upgrade.ContractID, upgrade.Network, upgrade.OldWasmHash, upgrade.NewWasmHash),
		})
	}

	body, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		// The struct is fixed and its strings are escaped by encoding/xml,
		// so marshalling cannot fail; report it as an internal fault rather
		// than pretending the feed is empty.
		s.log.ErrorContext(r.Context(), "encoding atom feed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// feedUpdated is the timestamp Atom requires at the feed level: when the
// newest event happened, or now when there are none yet.
func feedUpdated(upgrades []store.Upgrade) string {
	updated := time.Now().UTC()
	if len(upgrades) > 0 {
		updated = upgrades[0].ObservedAt.UTC()
	}
	return updated.Format(time.RFC3339)
}

// nextUpgradePage reports the following page when one exists.
func nextUpgradePage(filter store.UpgradeFilter, page *store.UpgradePage) (store.UpgradeFilter, bool) {
	seen := filter.Offset + len(page.Upgrades)
	if len(page.Upgrades) == 0 || seen >= page.Total {
		return store.UpgradeFilter{}, false
	}
	next := filter
	next.Offset = seen
	return next, true
}

// atomURL builds a link back to the feed, preserving the selection.
func atomURL(filter store.UpgradeFilter) string {
	values := url.Values{}
	if filter.Network != "" {
		values.Set("network", filter.Network)
	}
	if filter.Limit > 0 {
		values.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		values.Set("offset", strconv.Itoa(filter.Offset))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/api/upgrades.atom?" + encoded
	}
	return "/api/upgrades.atom"
}

// contractURL links an upgrade to the contract it is about.
func contractURL(upgrade store.Upgrade) string {
	return "/api/contracts/" + upgrade.ContractID + "?network=" + url.QueryEscape(upgrade.Network)
}

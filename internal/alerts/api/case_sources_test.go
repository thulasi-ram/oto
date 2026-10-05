package api

import (
	"net/http"
	"testing"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/test/contract/schema"
)

// ADR 0056 §1, "staleness is shown": a Case says who can still speak for it, so
// its screen can say whether it can expire and, when it cannot, why. These pin
// the `sources` member both case shapes carry.

// TestACaseDetailSaysWhoCanStillSpeakForIt.
//
// The promise: `GET /cases/{id}` carries `sources` — the live and removed counts
// on the Case's cluster, and the one live source with the §B.4 verdict and its
// max silence in seconds.
func TestACaseDetailSaysWhoCanStillSpeakForIt(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	resp := c.GET(casePath("")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getCase", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	src, ok := data["sources"].(map[string]any)
	if !ok {
		t.Fatalf("sources = %v, want an object: %s", data["sources"], resp)
	}
	if src["live"] != float64(1) || src["removed"] != float64(0) {
		t.Fatalf("live/removed = %v/%v, want 1/0", src["live"], src["removed"])
	}
	one, ok := src["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %v, want the one live source", src["source"])
	}
	if one["id"] != fxSourceID.String() || one["name"] != "alertmanager-prod-eu" {
		t.Fatalf("source = %v, want %s named alertmanager-prod-eu", one, fxSourceID)
	}
	if one["healthy"] != true {
		t.Fatalf("healthy = %v, want true", one["healthy"])
	}
	if one["max_silence_seconds"] != float64(86400) {
		t.Fatalf("max_silence_seconds = %v, want 86400 (a day, in seconds)", one["max_silence_seconds"])
	}
	if svc.calls["CaseCover"] != 1 {
		t.Fatalf("the cover was read %d time(s), want 1", svc.calls["CaseCover"])
	}
}

// TestACaseWhoseCoverCannotBeReadSaysNullNotNoSource.
//
// The promise: a failed read is `sources: null` and the page still answers. A
// zeroed object would read as "no live source", which the screen renders as a
// reason the Case cannot expire — a wrong explanation, where null is none.
func TestACaseWhoseCoverCannotBeReadSaysNullNotNoSource(t *testing.T) {
	t.Parallel()

	c, svc := newAlertsProbe(t)
	svc.failCover = errs.Internal("cover_failed", errs.ErrInternal)

	resp := c.GET(casePath("")).MustStatus(t, http.StatusOK)
	schema.Assert(t, "getCase", http.StatusOK, resp.Body())

	data, _ := resp.JSON(t)["data"].(map[string]any)
	v, present := data["sources"]
	if !present || v != nil {
		t.Fatalf("sources = %v (present %v), want an explicit null", v, present)
	}
}

// TestEveryCaseListRowSaysWhoCanStillSpeakForIt.
//
// The promise: every `GET /cases` row carries `sources`, read for the whole page
// at once — including the two shapes that hold or end a Case: a cluster whose
// only source was removed (`live: 0, removed: 1`) and an HA pair (`live: 2`),
// neither of which names a single source.
func TestEveryCaseListRowSaysWhoCanStillSpeakForIt(t *testing.T) {
	t.Parallel()

	c, _ := newAlertsProbe(t)
	resp := c.GET("/cases").MustStatus(t, http.StatusOK)
	schema.Assert(t, "listCases", http.StatusOK, resp.Body())

	rows, _ := resp.JSON(t)["data"].([]any)
	byID := map[string]map[string]any{}
	for _, row := range rows {
		r, _ := row.(map[string]any)
		src, ok := r["sources"].(map[string]any)
		if !ok {
			t.Fatalf("row %v carries no sources object", r["id"])
		}
		id, _ := r["id"].(string)
		byID[id] = src
	}

	removed := byID[fxEndedOccID.String()]
	if removed["live"] != float64(0) || removed["removed"] != float64(1) || removed["source"] != nil {
		t.Fatalf("removed-source row = %v, want live 0, removed 1, source null", removed)
	}
	ha := byID[fxSuppOccID.String()]
	if ha["live"] != float64(2) || ha["source"] != nil {
		t.Fatalf("HA row = %v, want live 2 and no single source", ha)
	}
}

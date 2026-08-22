package template

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// Input is everything a Wording can reach, and nothing else.
//
// ⛔ IT IS BUILT BY HAND, FIELD BY FIELD, AND THAT IS THE POINT. Liquid reflects
// into Go structs: a spike passing a struct into bindings rendered an
// unexported-by-intent field via `{{ s.Token }}` (see spike-liquid evidence in
// ADR 0037). Passing *domain.NotificationView would therefore expose every field
// oto ever adds to it, forever, with no review step. Building a projection turns
// "what can a Wording reach" from a reflection question into a diff on this file.
//
// ⚠️ NESTED MAPS, NOT A FLAT ONE, AND THE ADR'S PROPERTY SURVIVES. ADR 0037 says
// "a flat map[string]any of scalars". The hazard it was closing is REFLECTION INTO
// GO STRUCTS, not depth — a map[string]any whose leaves are scalars is not a
// struct and exposes nothing but what this file writes into it. Depth buys
// `{{ alert.name }}` over `{{ alert_name }}`, which is the difference between a
// language an operator can guess and one they must look up. The property is held
// mechanically instead of by flatness: TestInputHoldsOnlyScalars walks the
// tree and fails on any leaf that is not a string, number, bool or nil.
type Input map[string]any

// BuildInput projects a NotificationView into the read set a Wording is allowed.
//
// Every string that came from outside oto — a label, an annotation, an enrichment
// payload, an actor's label — is sanitised here, at the boundary, so no filter and
// no Dialect has to remember to. Timestamps become time marks, which each provider
// spells for itself.
func BuildInput(v *domain.NotificationView, at time.Time, f Format) (Input, map[string]string) {
	b := newBinder(f)
	in := Input{
		"reason":        b.text(v.Reason),
		"notifications": v.Notifications,
		"comment":       b.text(v.Comment),
		"snoozed":       v.SnoozedUntil != nil,
		"org": map[string]any{
			"name": b.text(v.Org.Name),
			"slug": b.text(v.Org.Slug),
		},
	}
	if v.SnoozedUntil != nil {
		in["snoozed_until"] = stamp(*v.SnoozedUntil)
	}

	g := v.Group
	in["group"] = map[string]any{
		"title": b.text(g.Title), "state": b.text(g.State), "severity": b.text(g.Severity),
		"receiver": b.text(g.Receiver), "cluster": b.text(g.ClusterKey),
		"generation":       g.Generation,
		"firing_count":     g.FiringCount,
		"resolved_count":   g.ResolvedCount,
		"suppressed_count": g.SuppressedCount,
		"expired_count":    g.ExpiredCount,
		"acked_count":      g.AckedCount,
		"total_count":      g.TotalCount,
		"started_at":       stamp(g.StartedAt),
		"first_seen_at":    stamp(g.FirstSeenAt),
		"last_activity_at": stamp(g.LastActivityAt),
		// firing_for is oto's own fact and one of the three the residue table says
		// only oto can put in a sentence. It is measured from upstream's startsAt,
		// never from when oto first heard, because the gap between those was
		// twenty-one minutes in the first live run.
		"firing_for": durationOf(g.StartedAt, at),
	}
	in["labels"] = b.stringMap(g.GroupLabels)
	in["label_list"] = b.pairList(g.GroupLabels)
	// ⭐ THE COLLECTIONS THAT USED TO BE REFUSED. `members` and `trail` were two
	// of the four stanzas the old per-slot design could not let anyone customise,
	// and the reason was always the same: they are sequences and the template
	// language had no way to walk one. `{% for %}` exists now, so they are
	// bound as ORDERED SLICES and a template can finally say what is in them.
	//
	// ⛔ ORDERED SLICES, NOT MAPS, EVERYWHERE. Go randomises map iteration; oto
	// hashes the rendered payload to suppress no-op `chat.update` calls; a
	// wobbling hash re-sends the same card forever. asList refuses a bare map at
	// the tag, and this is the other half of that rule — the shapes worth
	// iterating are all offered as slices, so nobody has to reach for a map.
	in["members"] = map[string]any{"count": len(v.Alerts), "list": b.memberList(v.Alerts)}
	in["trail"] = map[string]any{"count": len(v.Trail), "list": b.trailList(v.Trail)}

	focus := v.Focus
	if focus == nil && len(v.Alerts) > 0 {
		focus = &v.Alerts[0]
	}
	if focus != nil {
		in["alert"] = map[string]any{
			"name": b.text(focus.AlertName), "severity": b.text(focus.Severity),
			"service": b.text(focus.Service), "namespace": b.text(focus.Namespace),
			"cluster": b.text(focus.ClusterKey),
			"state":   b.text(focus.State), "ack_state": b.text(focus.AckState),
			// total_cases is residue item 3: "4th time this week" is a sentence
			// Prometheus cannot write because Prometheus does not count episodes.
			"total_cases":   focus.TotalCases,
			"flapping":      focus.IsFlapping,
			"first_seen_at": stamp(focus.FirstSeenAt),
			"last_seen_at":  stamp(focus.LastSeenAt),
			"value":         floatOrNil(focus.Value),
		}
		// ⚠️ ANNOTATIONS ARE MERGED THE WAY THE RENDERER RESOLVES THEM, not taken
		// from the focus alone. `slack.annotation()` looks at the focused alert, then
		// the group, then the RULE snapshot — so a title Wording that took only the
		// focus could not reach the very `summary` the built-in subtitle it is
		// replacing was showing. Precedence runs lowest-first here so the focus wins,
		// matching the renderer.
		merged := map[string]string{}
		if v.Rule != nil {
			for k, val := range v.Rule.Annotations {
				merged[k] = val
			}
		}
		for k, val := range focus.Annotations {
			merged[k] = val
		}
		in["annotations"] = b.stringMap(merged)
		in["annotation_list"] = b.pairList(merged)
		if len(focus.Labels) > 0 {
			in["labels"] = b.stringMap(focus.Labels)
			in["label_list"] = b.pairList(focus.Labels)
		}
	}

	if c := v.Case; c != nil {
		in["case"] = map[string]any{
			"state": b.text(c.State), "ack_state": b.text(c.AckState),
			"seq":      c.Seq,
			"duration": humanise(c.Duration),
			// acked_by is the actor label the ack surface already resolved. It is a
			// LABEL, never an id, and oto learns nothing about the human behind it.
			"acked_by":           b.text(c.AckedByLabel),
			"ack_note":           b.text(c.AckNote),
			"resolve_reason":     b.text(c.ResolveReason),
			"suppression_reason": b.text(c.SuppressionReason),
			"started_at":         stamp(c.StartedAt),
			"acked_at":           stampPtr(c.AckedAt),
			"ended_at":           stampPtr(c.EndedAt),
		}
	}
	if r := v.Rule; r != nil {
		in["rule"] = map[string]any{
			"name": b.text(r.Name), "file": b.text(r.File), "group": b.text(r.Group),
			"expr": b.text(r.Expr), "for": humanise(r.For),
			"keep_firing_for":  humanise(r.KeepFiringFor),
			"origin":           b.text(r.Origin),
			"match_confidence": b.text(r.MatchConfidence),
			"captured_at":      stamp(r.CapturedAt),
		}
	}
	if rc := v.RuleChange; rc != nil {
		in["rule_change"] = map[string]any{
			"changed":       true,
			"expr_changed":  rc.ExprChanged,
			"for_changed":   rc.ForChanged,
			"previous_expr": b.text(rc.PreviousExpr), "new_expr": b.text(rc.NewExpr),
			"previous_for": humanise(rc.PreviousFor), "new_for": humanise(rc.NewFor),
			"labels_changed":      len(rc.LabelDiff),
			"annotations_changed": len(rc.AnnotationDiff),
			"captured_at":         stamp(rc.PreviousCapturedAt),
		}
	} else {
		in["rule_change"] = map[string]any{"changed": false}
	}
	if a := v.Actor; a != nil {
		in["actor"] = map[string]any{"label": b.text(a.Label), "kind": b.text(a.Kind)}
	}
	if d := v.Digest; d != nil {
		dm := map[string]any{"count": d.Count}
		// ⚠️ A DIGEST WRITTEN BEFORE MIGRATION 00070 HAS A ZERO SPAN, AND A WORDING
		// MUST BE ABLE TO SAY SO RATHER THAN INVENT IT. The keys are absent, not
		// zero, so `{{ digest.covered_from | default: "an unknown period" }}` is the
		// natural thing to write and the honest thing to render.
		if !d.CoveredFrom.IsZero() && !d.CoveredTo.IsZero() {
			dm["covered_from"] = stamp(d.CoveredFrom)
			dm["covered_to"] = stamp(d.CoveredTo)
		}
		in["digest"] = dm
	}

	// ⭐ THIS IS WHERE THE `alert.history` PAYLOAD FINALLY REACHES A SURFACE. The
	// design note's residue item 3 is that an Enricher's payload renders nowhere:
	// enrichmentSummary prints a count and a label and drops the result. A Wording
	// cannot iterate — there is no {% for %} — so the payload is exposed by NAME,
	// which is the only shape a loop-free language can consume, and is also the
	// shape that makes a Wording's read set statically analysable.
	if len(v.Enrichments) > 0 {
		en := map[string]any{}
		// Sorted, first-writer-wins, for the reason stringMap states: an enricher
		// named `alert.history` and one named `alert_history` both normalise to
		// `alert_history`, and Go's map order would pick a different winner per
		// render.
		for _, name := range sortedKeys(v.Enrichments) {
			e := v.Enrichments[name]
			entry := map[string]any{
				"status":      b.text(e.Status),
				"error":       b.text(e.Error),
				"warnings":    len(e.Warnings),
				"computed_at": stamp(e.ComputedAt),
			}
			for _, k := range sortedKeys(e.Payload) {
				if sc, ok := b.scalar(e.Payload[k]); ok {
					if key := safeKey(k); !hasKey(entry, key) {
						entry[key] = sc
					}
				}
			}
			if key := safeKey(name); !hasKey(en, key) {
				en[key] = entry
			}
		}
		in["enrichment"] = en
	}

	// ⭐ LINKS ARE BOUND AS HANDLES, NOT AS URLs, AND THAT IS THE SECURITY
	// BOUNDARY OF THE WHOLE CARD FORMAT. Once Liquid has flattened the template
	// into one string, `https://oto.example/case/1` and an address an alert
	// label smuggled in look identical. A handle is minted AFTER sanitise() has
	// stripped the private-use area from the source and from every value, so it
	// is the one thing in the output that only oto could have put there.
	in["links"] = b.linkNamespace(v.Links)

	// The actions token. Binding it rather than scanning the source for the
	// literal `{{ actions }}` means `{% if %}` around it works for free.
	in["actions"] = sentinelActions

	return in, b.links
}

// pairList renders a name/value map as an ordered slice, sorted by name.
//
// It keeps the ORIGINAL name rather than safeKey's normalisation, because a list
// is read by a human and `cases.7d` is what the label is actually called. The
// by-name map keeps the normalised form, for `{{ labels.cases_7d }}`.
func (b *binder) pairList(m map[string]string) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, map[string]any{"name": b.text(k), "value": b.text(m[k])})
	}
	return out
}

func (b *binder) memberList(as []domain.AlertView) []map[string]any {
	out := make([]map[string]any, 0, len(as))
	for i := range as {
		a := &as[i]
		out = append(out, map[string]any{
			"name": b.text(a.AlertName), "severity": b.text(a.Severity),
			"state": b.text(a.State), "ack_state": b.text(a.AckState),
			"service": b.text(a.Service), "namespace": b.text(a.Namespace),
			"cluster":       b.text(a.ClusterKey),
			"first_seen_at": stamp(a.FirstSeenAt), "last_seen_at": stamp(a.LastSeenAt),
			"total_cases": a.TotalCases, "flapping": a.IsFlapping,
			"value": floatOrNil(a.Value),
		})
	}
	return out
}

func (b *binder) trailList(ts []domain.TrailEntry) []map[string]any {
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{
			"kind": b.text(t.Kind), "at": stamp(t.At), "actor": b.text(t.Actor),
		})
	}
	return out
}

// binder carries the per-format escaping decision and the link table.
type binder struct {
	esc   func(string) string
	links map[string]string
}

// newBinder picks the escaping a format needs.
//
// ⛔ card ESCAPES EVERY VALUE AND THERE IS NO OPT-OUT. An alert label is
// attacker-influenced — anyone who can fire a metric can write one — and a value
// that cannot produce syntax cannot produce structure, a link, a mention, or a
// forged handle. That single rule is why the card format needs no raw-output
// mechanism and no taint tracking.
//
// ⚠️ text AND raw DO NOT MARKDOWN-ESCAPE, because neither is parsed as Markdown.
// A `text` template's output meets Spell, which escapes for the PROVIDER; a
// `raw` template's output is JSON, where markdown backslashes would be corruption
// and `| json` is the author's tool. Both are still sanitised.
func newBinder(f Format) *binder {
	esc := func(s string) string { return s }
	if f == FormatCard {
		esc = escapeMarkdown
	}
	return &binder{esc: esc, links: map[string]string{}}
}

func (b *binder) text(s string) string { return b.esc(sanitise(s)) }

// stringMap normalises label names into Liquid identifiers, DETERMINISTICALLY.
//
// ⛔ TWO LABELS CAN NORMALISE ONTO ONE KEY, AND THE LOSER MUST NOT BE CHOSEN BY GO'S
// MAP ORDER. `cases.7d` and `cases_7d` both become `cases_7d`; ranging a Go map
// visits them in a random order, so the same NotificationView rendered twice would
// produce two different cards. That is not merely untidy: SPEC §F.1 requires the
// renderer to be a pure function, and oto hashes the rendered payload to suppress
// no-op `chat.update` calls — a wobbling hash re-sends the same card forever. It is
// the same failure asList refuses a bare map to avoid, arriving by another door.
//
// The rule is: sorted by the ORIGINAL name, first writer wins. Arbitrary, stated,
// and above all the same on every render.
func (b *binder) stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		if key := safeKey(k); !hasKey(out, key) {
			out[key] = b.text(m[k])
		}
	}
	return out
}

// link registers an address under a key and returns the handle that stands for
// it. An empty address registers nothing, so `{{ links.runbook }}` on a view with
// no runbook renders to a handle the parser resolves to nothing — which drops the
// link and keeps the words.
func (b *binder) link(key, addr string) any {
	addr = strings.TrimSpace(sanitise(addr))
	if addr == "" {
		return nil
	}
	b.links[key] = addr
	return linkOpen + key + linkShut
}

// linkNamespace binds every deep link a card may offer.
func (b *binder) linkNamespace(l domain.Links) map[string]any {
	out := map[string]any{}
	for key, addr := range map[string]string{
		"group": l.Group, "alert": l.Alert, "timeline": l.Timeline,
		"prometheus": l.Prometheus, "alertmanager": l.Alertmanager,
		"silence": l.AlertmanagerSilenceNew, "runbook": l.Runbook,
		"dashboard": l.GrafanaDashboard, "panel": l.GrafanaPanel,
	} {
		if h := b.link(key, addr); h != nil {
			out[key] = h
		}
	}
	return out
}

// stamp turns an instant into a time mark carrying both the epoch and oto's own
// UTC rendering, so a Dialect can pick whichever its provider supports.
func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return string(markTimeOpen) + strconv.FormatInt(u.Unix(), 10) + string(markTimeSep) +
		u.Format("2006-01-02 15:04 MST") + string(markTimeClose)
}

func stampPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}

// durationOf is the elapsed time between a start and now, absent when unknown.
func durationOf(from, to time.Time) any {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil
	}
	return humanise(to.Sub(from))
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// scalar admits the leaf types a template may see, plus a LIST of them.
//
// ⭐ THE LIST CASE IS NEW, AND IT IS THE POINT OF HAVING `{% for %}`. An
// enrichment payload is arbitrary customer JSON; before whole-message templating
// there was no construct that could read an array, so dropping one cost nothing.
// Now `{% for host in enrichment.alert_history.hosts %}` is the kind of sentence
// this whole feature exists to make possible.
//
// ⛔ AN OBJECT IS STILL DROPPED, AND A LIST OF OBJECTS WITH IT. Iterating a map
// would put Go's deliberately-randomised key order onto a card whose hash decides
// whether oto re-sends it — the same rule asList states, enforced at the boundary
// so a payload cannot smuggle a map past it.
func (b *binder) scalar(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		return b.text(t), true
	case bool:
		return t, true
	case float64, float32, int, int32, int64:
		return t, true
	case nil:
		return nil, true
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			sc, ok := b.scalar(e)
			if !ok {
				return nil, false
			}
			out = append(out, sc)
		}
		return out, true
	}
	return nil, false
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// text sanitises a value that came from outside oto.

// safeKey makes a label or enricher name reachable as a Liquid identifier without
// letting its characters mean anything. A name oto cannot spell is still present
// under its normalised key rather than silently missing.
func safeKey(k string) string {
	k = sanitise(strings.ToLower(strings.TrimSpace(k)))
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, k)
}

package service

import (
	"sort"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/enrichment/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// CacheSeeder is an OPTIONAL extension an Enricher may implement.
//
// domain.Enricher returns its cache key on the way OUT, inside the Result,
// which is the right shape for writing the cache but useless for reading it:
// by the time the key is known the work is already done. An Enricher that can
// name its inputs BEFORE doing the work implements this, and the pipeline then
// consults `enrichment_cache` first and may skip the call entirely.
//
// The seed is a stable rendering of the enricher's own inputs. It is hashed and
// namespaced by org and version before it becomes a cache key, so it may be
// arbitrarily long and may contain label values.
type CacheSeeder interface {
	CacheSeed(s *domain.Subject) string
}

// Registry is the boot-time set of Enrichers.
//
// Order is DETERMINISTIC by construction: entries are kept sorted by
// (phase, name), so two runs over the same registry always dispatch, record and
// report in the same order. That is not tidiness — a pipeline whose result
// ordering depends on registration order or map iteration produces diffs that
// cannot be golden-tested and Slack cards that reshuffle between two identical
// alerts.
type Registry struct {
	byName   map[string]domain.Enricher
	ordered  []domain.Enricher
	disabled map[string]bool
}

// NewRegistry builds a registry from the enrichers wired in
// `internal/app/enrichers.go`.
//
// A duplicate or malformed name is an error at BOOT, not a surprise at 3am:
// enrichments_name_ck would reject the row anyway, and discovering that on the
// first fire of a new alert is the worst possible moment.
func NewRegistry(enrichers ...domain.Enricher) (*Registry, error) {
	r := &Registry{
		byName:   make(map[string]domain.Enricher, len(enrichers)),
		disabled: map[string]bool{},
	}
	for _, e := range enrichers {
		if e == nil {
			return nil, errs.New(errs.KindInternal, "enrichment_nil_enricher",
				"a nil Enricher was registered")
		}
		name := e.Name()
		if !domain.ValidEnricherName(name) {
			return nil, errs.Newf(errs.KindInternal, "enrichment_bad_enricher_name",
				"enricher name %q must be dotted and lowercase (enrichments_name_ck)", name)
		}
		if e.Version() < 1 {
			return nil, errs.Newf(errs.KindInternal, "enrichment_bad_enricher_version",
				"enricher %q must declare a version >= 1", name)
		}
		if !e.Phase().Valid() {
			return nil, errs.Newf(errs.KindInternal, "enrichment_bad_enricher_phase",
				"enricher %q declared an unknown phase", name)
		}
		if _, dup := r.byName[name]; dup {
			return nil, errs.Newf(errs.KindInternal, "enrichment_duplicate_enricher",
				"enricher %q is registered twice", name)
		}
		r.byName[name] = e
		r.ordered = append(r.ordered, e)
	}

	sort.SliceStable(r.ordered, func(i, j int) bool {
		if r.ordered[i].Phase() != r.ordered[j].Phase() {
			return r.ordered[i].Phase() < r.ordered[j].Phase()
		}
		return r.ordered[i].Name() < r.ordered[j].Name()
	})
	return r, nil
}

// Disable turns off named enrichers at runtime.
//
// Every enricher is independently disableable BY DESIGN: they are the part of
// oto most likely to misbehave against a specific customer's Prometheus, and
// the answer to "your rule lookup is hammering our API" has to be a config
// change, not a redeploy. A disabled enricher is simply absent — its results
// are neither computed nor recorded, so nothing in the UI claims it failed.
func (r *Registry) Disable(names ...string) {
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" {
			r.disabled[n] = true
		}
	}
}

// Enabled reports whether an enricher will run.
func (r *Registry) Enabled(name string) bool {
	_, ok := r.byName[name]
	return ok && !r.disabled[name]
}

// Get returns one registered enricher.
func (r *Registry) Get(name string) (domain.Enricher, bool) {
	e, ok := r.byName[name]
	return e, ok
}

// All returns every registered enricher in deterministic order.
func (r *Registry) All() []domain.Enricher {
	out := make([]domain.Enricher, len(r.ordered))
	copy(out, r.ordered)
	return out
}

// Select resolves the enrichers one run should dispatch.
//
// With no names, it is every ENABLED enricher declaring this phase. With names,
// it is exactly those, regardless of the phase they declare — that is how an
// inline enricher that blew the budget is re-run in the async pass without
// having to lie about its own phase.
func (r *Registry) Select(phase domain.Phase, names []string) []domain.Enricher {
	if len(names) == 0 {
		out := make([]domain.Enricher, 0, len(r.ordered))
		for _, e := range r.ordered {
			if e.Phase() == phase && !r.disabled[e.Name()] {
				out = append(out, e)
			}
		}
		return out
	}

	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[strings.TrimSpace(n)] = true
	}
	out := make([]domain.Enricher, 0, len(names))
	for _, e := range r.ordered {
		if wanted[e.Name()] && !r.disabled[e.Name()] {
			out = append(out, e)
		}
	}
	return out
}

// timeoutOf returns the enricher's declared per-call timeout, defaulted and
// bounded. An enricher that declares none gets the default rather than the
// whole phase budget: one slow enricher must not be able to spend everyone
// else's time.
func timeoutOf(e domain.Enricher, budget time.Duration) time.Duration {
	t := e.Timeout()
	if t <= 0 {
		t = domain.DefaultEnricherTimeout
	}
	if budget > 0 && t > budget {
		t = budget
	}
	return t
}

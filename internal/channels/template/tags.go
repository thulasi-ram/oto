package template

import (
	"strings"

	"github.com/osteele/liquid"
	"github.com/osteele/liquid/render"
)

// The control flow a card template gets, implemented here rather than imported.
//
// ⭐ THIS IS WHY NewBasicEngine SURVIVED THE PIVOT TO WHOLE-MESSAGE TEMPLATING.
// Whole-message authoring needs iteration — nobody can render a label list
// without it — and liquid's standard tags are ALL OR NOTHING from outside the
// package: `Engine.cfg` is unexported, so `tags.AddStandardTags` is unreachable
// and a basic engine can never be given `{% for %}`. The alternative was
// `NewEngine()`, which brings thirteen tags including `include` and `render`
// (both of which read from a template store) and forty filters, none of them
// chosen, and none of the filters removable — there is no UnregisterFilter.
//
// ⛔ SO OTO WRITES THE TWO IT WANTS, AND THE ITERATION BUDGET BECOMES REAL. A
// cap enforced inside this loop counts actual iterations. The alternative was
// scanning the source for `{% for %}` nesting depth and reasoning about the
// worst case, which is a heuristic standing in for a measurement.
const (
	// MaxIterations bounds ONE loop. Nested loops multiply, which is why
	// MaxTotalIterations exists as well.
	MaxIterations = 250
	// MaxTotalIterations bounds every loop in one render, together.
	MaxTotalIterations = 2000
)

// registerTags gives the engine oto's own control flow.
func registerTags(e *liquid.Engine) {
	e.RegisterBlock("for", forBlock)
	e.RegisterBlock("if", ifBlock(true))
	e.RegisterBlock("unless", ifBlock(false))
}

// forBlock implements `{% for x in expr %}…{% endfor %}`.
//
// ⚠️ THERE IS NO `{% else %}`, because RegisterBlock cannot declare a clause.
// `{% unless %}` covers the case an else-branch usually serves, and saying so is
// better than the alternative, which was thirteen upstream tags.
func forBlock(ctx render.Context) (string, error) {
	name, expr, ok := splitIn(ctx.TagArgs())
	if !ok {
		return "", ctx.Errorf("`{%% for %%}` reads `{%% for item in collection %%}`, and %q is not that", ctx.TagArgs())
	}
	val, err := ctx.EvaluateString(expr)
	if err != nil {
		return "", ctx.WrapError(err)
	}
	items, ok := asList(val)
	if !ok {
		// A loop over a scalar is a mistake, but not one worth failing a
		// delivery for: nothing is a sound rendering of "no items".
		return "", nil
	}
	if len(items) > MaxIterations {
		items = items[:MaxIterations]
	}

	var b strings.Builder
	for i, it := range items {
		if !chargeIteration(ctx) {
			break
		}
		ctx.Set(name, it)
		ctx.Set("forloop", map[string]any{
			"index0": i, "index": i + 1,
			"first": i == 0, "last": i == len(items)-1,
			"length": len(items),
		})
		if rerr := ctx.RenderChildren(&b); rerr != nil {
			return "", rerr
		}
		if b.Len() > MaxOutputBytes {
			return "", ctx.Errorf("a loop produced more than %d bytes", MaxOutputBytes)
		}
	}
	return b.String(), nil
}

// ifBlock implements `{% if expr %}` and, inverted, `{% unless expr %}`.
func ifBlock(want bool) func(render.Context) (string, error) {
	return func(ctx render.Context) (string, error) {
		val, err := ctx.EvaluateString(ctx.TagArgs())
		if err != nil {
			return "", ctx.WrapError(err)
		}
		if truthy(val) != want {
			return "", nil
		}
		var b strings.Builder
		if rerr := ctx.RenderChildren(&b); rerr != nil {
			return "", rerr
		}
		return b.String(), nil
	}
}

// iterationBudget counts iterations across every loop in one render.
//
// It is carried in the bindings rather than in a field, because a Template is
// shared by every concurrent delivery and a counter on the struct would be a
// data race and a cross-tenant leak at once.
const iterationBudgetKey = "__oto_iterations"

func chargeIteration(ctx render.Context) bool {
	n, _ := ctx.Get(iterationBudgetKey).(*int)
	if n == nil {
		return true
	}
	if *n <= 0 {
		return false
	}
	*n--
	return true
}

// withBudget returns bindings carrying a fresh per-render iteration budget.
func withBudget(in Input) liquid.Bindings {
	b := make(liquid.Bindings, len(in)+1)
	for k, v := range in {
		b[k] = v
	}
	n := MaxTotalIterations
	b[iterationBudgetKey] = &n
	return b
}

// splitIn parses `item in collection`.
func splitIn(args string) (name, expr string, ok bool) {
	f := strings.Fields(strings.TrimSpace(args))
	if len(f) < 3 || f[1] != "in" {
		return "", "", false
	}
	return f[0], strings.Join(f[2:], " "), true
}

// asList accepts the shapes BuildInput actually produces.
//
// ⛔ A MAP IS NOT A LIST AND NEVER WILL BE. oto hashes the rendered payload to
// suppress no-op `chat.update` calls, so iteration order has to be stable across
// renders of the same view — and Go's map order is deliberately not. This is the
// one rule that survived the pivot intact: BuildInput exposes ordered slices for
// everything a template can loop over, and a bare map here renders nothing
// rather than rendering differently each time and re-sending the card forever.
func asList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// truthy follows Liquid: only nil and false are falsy.
//
// ⚠️ AN EMPTY STRING IS TRUTHY HERE, AS IN LIQUID, AND THAT WOULD BE A FOOTGUN
// IF BuildInput BOUND ONE. It does not: an absent or empty scalar is left out of
// the bindings entirely, so `{% if alert.summary %}` asks the question the author
// means without oto having to deviate from the syntax it borrowed.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	}
	return true
}

package slack

import (
	"errors"
	"sync"

	"github.com/thulasiram/oto/internal/channels/domain"
	"github.com/thulasiram/oto/internal/channels/render/wording"
)

// stanzas is the per-delivery seam between a customer's Wordings and oto's own
// strings. One is built at the top of a render and consulted at each Stanza.
//
// ⛔ IT NEVER DECIDES WHETHER A BLOCK EXISTS. `or` is called with the string Go
// already computed, and returns either the customer's or Go's — so a Wording can
// change what a block SAYS and can never change which blocks there are, how they
// are ordered, or what colour the attachment is. Every early return in root.go
// that drops an empty block still runs on Go's value.
type stanzas struct {
	in      wording.StanzaInput
	dialect wording.Dialect
	sources map[string]string
}

// newStanzas builds the seam. It is cheap when the customer has no Wordings,
// which is the overwhelmingly common case: no input projection is built at all.
func (r *Renderer) newStanzas(v *domain.NotificationView, o domain.RenderOptions) *stanzas {
	if len(o.Wordings) == 0 {
		return nil
	}
	return &stanzas{
		in:      wording.BuildInput(v, r.renderedAt(v)),
		dialect: wording.SlackDialect{},
		sources: o.Wordings,
	}
}

// or returns the customer's text for id, or builtin when there is no Wording, the
// Wording fails, or it renders empty.
//
// ⚠️ THE FALLBACK IS SILENT ON THE CARD AND LOUD IN THE LOGS' ABSENCE — which is a
// known gap, not a decision. A Wording that has been quietly failing for a week
// produces a correct card and no signal. The counter that would say so is
// oto_render_invalid_total's sibling and belongs with it; until then the preview
// endpoint is where an operator finds out.
func (s *stanzas) or(id wording.StanzaID, builtin string) string {
	if s == nil {
		return builtin
	}
	src, ok := s.sources[string(id)]
	if !ok || src == "" {
		return builtin
	}
	w, err := compiledWording(id, src)
	if err != nil {
		return builtin
	}
	out, err := w.Render(s.in, s.dialect)
	if err != nil {
		return builtin
	}
	return out
}

// compiledCache keeps parsed templates across deliveries.
//
// A Renderer is built once (`New(clk)`) and shared by every dispatch, and a
// customer's Wording changes rarely while their alerts do not — so parsing one
// per delivery would be pure waste. The cache is keyed by stanza and source, so a
// changed template is a different key and there is nothing to invalidate.
//
// ⚠️ IT DOES NOT MAKE THE RENDERER IMPURE. §F.1 requires that the renderer be a
// pure function of its input: the cache is transparent — same input, same output,
// no I/O, no clock — and a cold cache differs from a warm one only in speed.
var compiledCache sync.Map // stanza+"\x00"+source -> *wording.Wording or error

// errCacheCorrupt can only happen if something stores a third type in the cache,
// which nothing does. It exists so the read has no unchecked assertion.
var errCacheCorrupt = errors.New("compiled wording cache holds an unexpected type")

func compiledWording(id wording.StanzaID, src string) (*wording.Wording, error) {
	key := string(id) + "\x00" + src
	if hit, ok := compiledCache.Load(key); ok {
		if w, ok := hit.(*wording.Wording); ok {
			return w, nil
		}
		if err, ok := hit.(error); ok {
			return nil, err
		}
		return nil, errCacheCorrupt
	}
	w, err := wording.Compile(id, src)
	if err != nil {
		compiledCache.Store(key, err)
		return nil, err
	}
	compiledCache.Store(key, w)
	return w, nil
}

// continuedMarker is §H.9's "this card replaces an earlier one" sentence. It is a
// constant because the footer re-attaches it after a Wording, and two spellings
// would make that check silently fail.
const continuedMarker = "_continued from an earlier card_"

// escapedOr returns the customer's text for id, or builds Go's.
//
// ⚠️ builtin IS A CLOSURE SO THAT THE ORDER READS CORRECTLY — the customer's text
// is preferred, Go's is what happens otherwise — and NOT, as this comment used to
// claim, to defer expensive composition. It does defer it for the rule stanza,
// which truncates a 900-rune expression inside the closure; it does NOT for the
// footer, which builds all nine of its parts before calling here and only defers
// the `Join`. A reason that is false for the example it names is worse than no
// reason, because the next reader restructures the caller to preserve it.
//
// ⚠️ NEITHER SIDE IS ESCAPED HERE. Go's builder escapes its own inputs as it always
// has; a Wording's output was escaped run-by-run by its Dialect, which is the only
// way oto's own <!date^…> tokens survive. Escaping the result again would break
// both.
func escapedOr(s *stanzas, id wording.StanzaID, builtin func() string) string {
	if s != nil {
		if out := s.or(id, ""); out != "" {
			return out
		}
	}
	return builtin()
}

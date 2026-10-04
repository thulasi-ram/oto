package domain

// THE INVESTIGATOR (ADR 0053 §1, §6; git-bug 180a525): a named, versioned
// configuration of a model-driven investigation.
//
// ⭐⭐ IT IS TWO HALVES, AND THE SPLIT IS §6's OWN SENTENCE. "Changing an Investigator's
// model, prompt or allowlist makes a new version; a Finding names the version that
// produced it." So a Version holds exactly those three things — the model endpoint it
// dials with the (base_url, model) identity pinned from it, the prompt, the allowlist —
// and is never edited. The Investigator itself holds what decides WHETHER and HOW FAR a
// run may go: `Enabled` (the per-Investigator kill switch) and the per-run Budgets. Those
// change in place, and every Investigation copies the Budgets it ran under, so a run is
// still readable after the numbers move.
//
// Bounds below mirror migration 00092's CHECKs; the API DTO tags are the third copy
// (CONTEXT.md §5b, R9).

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// Bounds mirroring migration 00092.
const (
	// MaxInvestigatorNameLength is `investigators_name_ck`'s `{0,62}` plus its first
	// letter.
	MaxInvestigatorNameLength = 63
	// MaxPromptLength is `investigator_versions_prompt_ck`, in characters.
	MaxPromptLength = 32768
	// MaxAllowlistTools is `investigator_versions_tools_ck`'s cardinality.
	MaxAllowlistTools = 64

	// MinStepBudget and MaxStepBudget bound the Tool calls one run may make
	// (`investigators_steps_ck`). One is the least that can look anything up; a
	// hundred is past any transcript a human will read to the end.
	MinStepBudget = 1
	MaxStepBudget = 100
	// MinTokenBudget and MaxTokenBudget bound input+output tokens for one run
	// (`investigators_tokens_ck`). Below a thousand the prompt alone is over budget;
	// two million is a ceiling on one run's bill, not a target.
	MinTokenBudget int64 = 1000
	MaxTokenBudget int64 = 2_000_000
	// MinWallSeconds and MaxWallSeconds bound one run's wall time
	// (`investigators_wall_ck`). The job's own timeout is derived from the maximum.
	MinWallSeconds = 10
	MaxWallSeconds = 1800
)

// The budgets a new Investigator gets when its writer names none. Generous enough to
// read a Case's timeline, its rule and its prior Findings and say something; small
// enough that a misconfigured prompt costs minutes, not an afternoon.
const (
	DefaultStepBudget        = 20
	DefaultTokenBudget int64 = 200_000
	DefaultWallSeconds       = 300
)

// EnricherPrefix is the Enrichment namespace a Finding is published under (ADR 0053
// §3): `investigator.<name>`.
const EnricherPrefix = "investigator."

var investigatorNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,62}$`)

// NewInvestigatorName checks a name against `investigators_name_ck`.
//
// ⭐ THE ALPHABET IS THE ENRICHER NAME'S, not a taste: the Finding is stored as the
// Enrichment `investigator.<name>`, whose CHECK admits lower-case letters and digits
// between dots and nothing else. A hyphen here would be an Investigator whose every
// Finding the enrichment store refuses.
func NewInvestigatorName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if !investigatorNamePattern.MatchString(name) {
		const msg = "an Investigator's name is 1 to 63 lower-case letters and digits, starting with a letter"
		return "", errs.Validation("investigator_name_invalid", msg,
			errs.Violation{Field: "name", Code: "pattern", Message: msg})
	}
	return name, nil
}

// Budgets are one run's three per-run controls (ADR 0053 §6). Hitting any of them
// ends the run `exhausted`, keeps whatever Finding it reached, and records which.
type Budgets struct {
	// MaxSteps is the most Tool calls one run may make.
	MaxSteps int
	// MaxTokens is the most input + output tokens one run may spend.
	MaxTokens int64
	// MaxWall is the longest one run may take, from the moment it starts.
	MaxWall time.Duration
}

// NewBudgets builds a Budgets inside 00092's bounds.
func NewBudgets(maxSteps int, maxTokens int64, maxWallSeconds int) (Budgets, error) {
	var v []errs.Violation
	if maxSteps < MinStepBudget || maxSteps > MaxStepBudget {
		v = append(v, errs.Violation{Field: "budgets/max_steps", Code: "out_of_range",
			Message: "1 to 100 Tool calls"})
	}
	if maxTokens < MinTokenBudget || maxTokens > MaxTokenBudget {
		v = append(v, errs.Violation{Field: "budgets/max_tokens", Code: "out_of_range",
			Message: "1000 to 2000000 input + output tokens"})
	}
	if maxWallSeconds < MinWallSeconds || maxWallSeconds > MaxWallSeconds {
		v = append(v, errs.Violation{Field: "budgets/max_wall_seconds", Code: "out_of_range",
			Message: "10 to 1800 seconds"})
	}
	if len(v) > 0 {
		return Budgets{}, errs.Validation("investigator_budgets_invalid",
			"an Investigator's budgets are outside the range oto will accept", v...)
	}
	return Budgets{MaxSteps: maxSteps, MaxTokens: maxTokens, MaxWall: time.Duration(maxWallSeconds) * time.Second}, nil
}

// DefaultBudgets is what an Investigator runs under when its writer named none.
func DefaultBudgets() Budgets {
	return Budgets{MaxSteps: DefaultStepBudget, MaxTokens: DefaultTokenBudget, MaxWall: DefaultWallSeconds * time.Second}
}

// WallSeconds is MaxWall as the column stores it.
func (b Budgets) WallSeconds() int { return int(b.MaxWall / time.Second) }

// Allowlist is the exact set of Tools an Investigator version may call (ADR 0053 §6:
// "named Tools, no wildcards"). A call outside it is refused and recorded as a Step.
//
// It is held sorted and de-duplicated, so two allowlists naming the same Tools in a
// different order are EQUAL — reordering a list is not a new version.
type Allowlist struct {
	names []string
}

// NewAllowlist builds an Allowlist.
//
// ⛔ NO WILDCARDS, AND NOT BY A BLOCKLIST OF `*`. Every name must be a Tool name the
// model protocol can carry (`^[A-Za-z0-9_-]{1,64}$`), an alphabet with no pattern
// syntax in it at all, so `*`, `oto_*` and `.*` are refused as malformed rather than
// interpreted. A repeated name is refused too: an allowlist that says one thing twice
// is one somebody typed in a hurry, and "did they mean something else?" is a question
// for them.
func NewAllowlist(names []string) (Allowlist, error) {
	if len(names) > MaxAllowlistTools {
		return Allowlist{}, errs.Validation("investigator_tools_invalid",
			"an Investigator may name at most 64 Tools",
			errs.Violation{Field: "tools", Code: "max_items", Message: "at most 64 Tools"})
	}
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if !toolNamePattern.MatchString(n) {
			return Allowlist{}, errs.Validation("investigator_tools_invalid",
				"a Tool is named exactly — letters, digits, underscores and hyphens; there are no wildcards",
				errs.Violation{Field: "tools", Code: "pattern", Message: "not an exact Tool name: " + quoteShort(raw)})
		}
		if _, dup := seen[n]; dup {
			return Allowlist{}, errs.Validation("investigator_tools_invalid",
				"a Tool is named once",
				errs.Violation{Field: "tools", Code: "unique", Message: n + " is named twice"})
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	slices.Sort(out)
	return Allowlist{names: out}, nil
}

// Names returns the allowlist, sorted.
func (a Allowlist) Names() []string { return append([]string(nil), a.names...) }

// Allows reports whether the exact name is on the list.
func (a Allowlist) Allows(name string) bool {
	_, ok := slices.BinarySearch(a.names, name)
	return ok
}

// Equal reports whether two allowlists name the same Tools.
func (a Allowlist) Equal(b Allowlist) bool { return slices.Equal(a.names, b.names) }

// NewPrompt checks an Investigator's prompt: 1 to 32768 characters once trimmed.
func NewPrompt(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(p); n == 0 || n > MaxPromptLength {
		const msg = "a prompt is 1 to 32768 characters"
		return "", errs.Validation("investigator_prompt_invalid", msg,
			errs.Violation{Field: "prompt", Code: "length", Message: msg})
	}
	return p, nil
}

// Version is one immutable version of an Investigator: what produced a Finding.
type Version struct {
	ID             uuid.UUID
	InvestigatorID uuid.UUID
	Number         int
	// ProviderID is the model endpoint row this version dials.
	ProviderID uuid.UUID
	// Model is that endpoint's identity, pinned when the version was written. A run
	// whose endpoint no longer reports it is refused as `model_changed`.
	Model     ModelIdentity
	Prompt    string
	Tools     Allowlist
	CreatedAt time.Time
}

// VersionSpec is the versioned half of an Investigator as a writer states it, before
// it is stored: the endpoint to dial (its identity is read from the endpoint row and
// pinned by the service), the prompt and the allowlist.
type VersionSpec struct {
	ProviderID uuid.UUID
	Prompt     string
	Tools      Allowlist
}

// NewVersionSpec validates the versioned half.
func NewVersionSpec(providerID uuid.UUID, prompt string, tools Allowlist) (VersionSpec, error) {
	if providerID == uuid.Nil {
		return VersionSpec{}, errs.Validation("investigator_model_required",
			"an Investigator names the model endpoint it uses",
			errs.Violation{Field: "model_provider_id", Code: "required", Message: "required"})
	}
	p, err := NewPrompt(prompt)
	if err != nil {
		return VersionSpec{}, err
	}
	return VersionSpec{ProviderID: providerID, Prompt: p, Tools: tools}, nil
}

// NeedsNewVersion reports whether writing spec over v — whose endpoint now reports
// `model` — changes what would produce a Finding: the endpoint row, its identity,
// the prompt or the allowlist (ADR 0053 §6). Anything else is not a new version.
func (v Version) NeedsNewVersion(spec VersionSpec, model ModelIdentity) bool {
	return v.ProviderID != spec.ProviderID || v.Model != model ||
		v.Prompt != spec.Prompt || !v.Tools.Equal(spec.Tools)
}

// Investigator is one stored Investigator with the version new runs use.
type Investigator struct {
	ID      uuid.UUID
	OrgID   uuid.UUID
	Name    string
	Enabled bool
	Budgets Budgets
	// MinInterval is the least time between two runs on one subject (ADR 0053 §6):
	// membership-change triggers inside it coalesce into one run. ⭐ IT IS NOT
	// VERSIONED: it decides WHEN a run may start, not what produced a Finding, so it
	// lives on this mutable half beside the kill switch and the budgets.
	MinInterval time.Duration
	// Current is the latest version: the one a new Investigation pins.
	Current   Version
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EnricherName is the Enrichment name its Findings are published under.
func (i Investigator) EnricherName() string { return EnricherPrefix + i.Name }

// InvestigatorDraft is a new Investigator, validated, before it is stored.
type InvestigatorDraft struct {
	Name        string
	Enabled     bool
	Budgets     Budgets
	MinInterval time.Duration
	Spec        VersionSpec
}

// InvestigatorChange is a write over an existing Investigator. A nil field is left as
// it is. The three versioned fields are folded over the current version into a whole
// VersionSpec (Apply), and the service writes a new version only when that differs.
type InvestigatorChange struct {
	Enabled     *bool
	Budgets     *Budgets
	MinInterval *time.Duration
	ProviderID  *uuid.UUID
	Prompt      *string
	Tools       *Allowlist
}

// TouchesVersion reports whether the change names any of the versioned fields.
func (c InvestigatorChange) TouchesVersion() bool {
	return c.ProviderID != nil || c.Prompt != nil || c.Tools != nil
}

// Apply folds the change's versioned fields over the current version.
func (c InvestigatorChange) Apply(cur Version) (VersionSpec, error) {
	provider, prompt, tools := cur.ProviderID, cur.Prompt, cur.Tools
	if c.ProviderID != nil {
		provider = *c.ProviderID
	}
	if c.Prompt != nil {
		prompt = *c.Prompt
	}
	if c.Tools != nil {
		tools = *c.Tools
	}
	return NewVersionSpec(provider, prompt, tools)
}

// quoteShort renders a refused value for a violation message without letting a
// pasted paragraph become the message.
func quoteShort(s string) string {
	const maxShown = 64
	if len(s) > maxShown {
		s = s[:maxShown] + "…"
	}
	return `"` + s + `"`
}

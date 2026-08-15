package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/notification/domain"
	"github.com/thulasiram/oto/internal/platform/errs"
)

// ⭐ `reasons` IS A SET, AND THIS FILE IS THE LAYER-3 HALF OF SAYING SO.
//
// CONTEXT.md §5b binds a bound to three places — the DTO `validate` tag, the
// domain constructor and the DDL CHECK — and requires them to be identical.
// Uniqueness on `reasons` lived in ONE of them: `unique` on both request DTOs,
// nothing in `Policy.Validate`, nothing in `policies_reasons_ck`. So a duplicate
// was refused for an HTTP body and storable by every other path — and the
// contract publishes `uniqueItems: true` on `PolicyDTO`, the RESPONSE, which
// makes a stored duplicate a row oto serves and its own generated client then
// refuses to parse.
//
// The other two layers are covered where they live: the CHECK by
// TestThePolicyReasonsCheckRefusesABag (repository, against a real Postgres),
// and the tag by the request going through `httpx.Bind`.

// validPolicy is a policy that passes Validate, so that a test which breaks one
// field is testing that field and not the six others.
func validPolicy() domain.Policy {
	return domain.Policy{
		OrgID:      uuid.New(),
		Name:       "page-sre",
		Priority:   domain.DefaultPolicyPriority,
		Enabled:    true,
		Reasons:    []domain.Reason{domain.ReasonFired, domain.ReasonAllResolved},
		ChannelIDs: []uuid.UUID{uuid.New()},
	}
}

// violation returns the first violation err carries for field with code, and
// whether there was one.
func violation(err error, field, code string) (errs.Violation, bool) {
	for _, v := range errs.ViolationsOf(err) {
		if v.Field == field && v.Code == code {
			return v, true
		}
	}
	return errs.Violation{}, false
}

func TestAPolicyCannotBeConstructedWithADuplicateReason(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.Reasons = []domain.Reason{domain.ReasonFired, domain.ReasonAcked, domain.ReasonFired}

	err := p.Validate()
	if err == nil {
		t.Fatal("a policy listing `fired` twice was accepted. The `unique` tag on the request " +
			"DTOs is then the only place the rule exists, so every writer that does not go " +
			"through httpx.Bind can store a value the response contract declares impossible")
	}

	v, ok := violation(err, "reasons", "duplicate_items")
	if !ok {
		t.Fatalf("the refusal does not name reasons/duplicate_items: %v — the code has to be "+
			"the one layer 1 emits for the same rule, or a duplicate comes back as two "+
			"different 422s depending on which layer caught it", errs.ViolationsOf(err))
	}
	if !strings.Contains(v.Message, "fired") {
		t.Fatalf("the violation does not say WHICH reason repeats: %q — a policy may carry "+
			"eighteen of them and the operator has to find the one", v.Message)
	}
}

// A list of six `fired`s is one mistake. Reporting it five times fills the
// settings form with five copies of one message against one control.
func TestARepeatedReasonIsReportedOncePerValue(t *testing.T) {
	t.Parallel()

	p := validPolicy()
	p.Reasons = []domain.Reason{
		domain.ReasonFired, domain.ReasonFired, domain.ReasonFired,
		domain.ReasonAcked, domain.ReasonAcked,
	}

	err := p.Validate()
	if err == nil {
		t.Fatal("a policy with two repeated reasons was accepted")
	}

	n := 0
	for _, v := range errs.ViolationsOf(err) {
		if v.Field == "reasons" && v.Code == "duplicate_items" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d duplicate violations, want 2 — one per repeated VALUE, not one per "+
			"repeated element", n)
	}
}

// The ceiling followed the wire down to 18 when uniqueness became real: a set
// drawn from an 18-value closed enum cannot reach 19, so 32 was a number no row
// could ever test. The two bounds are asserted together because they now share a
// justification — and because a Validate that only counted would let the whole
// enum through twice while calling it 36 reasons.
func TestTheReasonCeilingIsTheSizeOfTheReasonEnum(t *testing.T) {
	t.Parallel()

	all := domain.AllReasons()
	if domain.MaxPolicyReasons != len(all) {
		t.Fatalf("MaxPolicyReasons is %d and the Reason enum has %d values. Since 00046 the "+
			"column is a SET, so the most a policy can carry is the whole vocabulary once; "+
			"any other ceiling is a number nothing can reach or a bound that outlaws a legal "+
			"policy", domain.MaxPolicyReasons, len(all))
	}

	// Every reason at once is the largest legal policy, and it must be legal.
	p := validPolicy()
	p.Reasons = all
	if err := p.Validate(); err != nil {
		t.Fatalf("a policy reacting to the entire Reason vocabulary was refused: %v", err)
	}

	// One more element cannot be a new reason, so it is necessarily a repeat —
	// which is exactly why the ceiling and the set rule are the same statement.
	p.Reasons = append(append([]domain.Reason{}, all...), domain.ReasonFired)
	err := p.Validate()
	if err == nil {
		t.Fatal("a policy carrying 19 reasons was accepted; the 19th can only be a duplicate")
	}
	if _, ok := violation(err, "reasons", "duplicate_items"); !ok {
		t.Fatalf("the 19-element list was refused without naming the duplicate: %v",
			errs.ViolationsOf(err))
	}
}

// The rule is a SET, not an ORDER: [fired, acked] and [acked, fired] are both
// legal and both mean the same thing. 00046's fold relies on this — it keeps
// first-occurrence order rather than sorting, precisely because neither order is
// more correct than the other and an operator should find the list they wrote.
func TestDistinctReasonsAreAcceptedInAnyOrder(t *testing.T) {
	t.Parallel()

	for _, reasons := range [][]domain.Reason{
		{domain.ReasonFired, domain.ReasonAcked},
		{domain.ReasonAcked, domain.ReasonFired},
	} {
		p := validPolicy()
		p.Reasons = reasons
		if err := p.Validate(); err != nil {
			t.Fatalf("%v was refused: %v", reasons, err)
		}
	}
}

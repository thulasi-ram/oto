package service_test

// A SLACK MEMBER LINKS THEMSELVES WITH A CODE oto SHOWED ONLY THEM (git-bug a556a5c). These run over
// in-memory stores whose predicates mirror the SQL's — a live code is unconsumed, unexpired and
// under its presentation cap; an unlink names its user — so the service's decisions are what is
// under test. The SQL itself is exercised by `internal/app/slack_link_db_test.go`.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/authn"
	"github.com/thulasiram/oto/internal/platform/clock"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/id"
)

// ------------------------------------------------------------------ fakes

type fakeSlackIdentities struct {
	mu   sync.Mutex
	byID map[uuid.UUID]domain.SlackIdentity
}

func (f *fakeSlackIdentities) put(si domain.SlackIdentity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[si.ID] = si
}

func (f *fakeSlackIdentities) get(id uuid.UUID) domain.SlackIdentity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byID[id]
}

func (f *fakeSlackIdentities) Upsert(_ context.Context, _ db.TenantScope, si domain.SlackIdentity, _ time.Time) (domain.SlackIdentity, error) {
	f.put(si)
	return si, nil
}

func (f *fakeSlackIdentities) GetByID(_ context.Context, s db.TenantScope, id uuid.UUID) (domain.SlackIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	si, ok := f.byID[id]
	if !ok || si.OrgID != s.OrgID() {
		return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	return si, nil
}

func (f *fakeSlackIdentities) LockByID(ctx context.Context, s db.TenantScope, id uuid.UUID) (domain.SlackIdentity, error) {
	return f.GetByID(ctx, s, id)
}

func (f *fakeSlackIdentities) GetByUser(context.Context, db.TenantScope, uuid.UUID) (domain.SlackIdentity, error) {
	return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
}

func (f *fakeSlackIdentities) ListByUser(_ context.Context, s db.TenantScope, userID uuid.UUID) ([]domain.SlackIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.SlackIdentity{}
	for _, si := range f.byID {
		if si.OrgID == s.OrgID() && si.UserID == userID {
			out = append(out, si)
		}
	}
	return out, nil
}

func (f *fakeSlackIdentities) GetBySlackUser(_ context.Context, s db.TenantScope, team domain.SlackTeamID, member domain.SlackUserID) (domain.SlackIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, si := range f.byID {
		if si.OrgID == s.OrgID() && si.TeamID == team && si.SlackUserID == member {
			return si, nil
		}
	}
	return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
}

func (f *fakeSlackIdentities) Link(_ context.Context, s db.TenantScope, id, userID uuid.UUID, at time.Time) (domain.SlackIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	si, ok := f.byID[id]
	if !ok || si.OrgID != s.OrgID() {
		return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	si, err := si.Link(userID, at)
	if err != nil {
		return domain.SlackIdentity{}, err
	}
	f.byID[id] = si
	return si, nil
}

// Unlink mirrors `unlinkSlackIdentitySQL`'s WHERE: only an identity linked to THIS user.
func (f *fakeSlackIdentities) Unlink(_ context.Context, s db.TenantScope, id, userID uuid.UUID) (domain.SlackIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	si, ok := f.byID[id]
	if !ok || si.OrgID != s.OrgID() || si.UserID != userID {
		return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
	}
	si = si.Unlink()
	f.byID[id] = si
	return si, nil
}

func (f *fakeSlackIdentities) ResolveBySlackUser(context.Context, domain.SlackTeamID, domain.SlackUserID) (domain.SlackIdentity, error) {
	return domain.SlackIdentity{}, errs.NotFound("slack_identity_not_found", "no such slack identity")
}

type fakeCode struct {
	identity      uuid.UUID
	org           uuid.UUID
	hash          domain.TokenHash
	expires       time.Time
	presentations int
	consumed      bool
}

type fakeSlackLinks struct {
	mu       sync.Mutex
	codes    map[uuid.UUID]*fakeCode     // by identity: one live code each
	attempts map[uuid.UUID][]fakeAttempt // by user: the reserved attempts, in order
	facts    []domain.SlackLinkFact
}

func (f *fakeSlackLinks) IssueCode(_ context.Context, s db.TenantScope, identityID uuid.UUID, hash domain.TokenHash, _, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[identityID] = &fakeCode{identity: identityID, org: s.OrgID(), hash: hash, expires: expiresAt}
	return nil
}

// live mirrors the SQL's liveness predicate.
func (f *fakeSlackLinks) live(s db.TenantScope, hash domain.TokenHash, now time.Time) *fakeCode {
	for _, c := range f.codes {
		if c.org == s.OrgID() && c.hash == hash && !c.consumed && c.expires.After(now) &&
			c.presentations < domain.SlackLinkCodeMaxPresentations {
			return c
		}
	}
	return nil
}

func (f *fakeSlackLinks) PresentCode(_ context.Context, s db.TenantScope, hash domain.TokenHash, now time.Time) (uuid.UUID, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.live(s, hash, now)
	if c == nil {
		return uuid.Nil, time.Time{}, domain.SlackLinkCodeInvalid()
	}
	c.presentations++
	return c.identity, c.expires, nil
}

func (f *fakeSlackLinks) ConsumeCode(_ context.Context, s db.TenantScope, hash domain.TokenHash, _ uuid.UUID, now time.Time) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.live(s, hash, now)
	if c == nil {
		return uuid.Nil, domain.SlackLinkCodeInvalid()
	}
	c.presentations++
	c.consumed = true
	return c.identity, nil
}

type fakeAttempt struct {
	id uuid.UUID
	at time.Time
}

// ReserveAttempt mirrors the SQL: under the (one) lock, prune, count, refuse at the limit, else
// record.
func (f *fakeSlackLinks) ReserveAttempt(_ context.Context, _ db.TenantScope, id, userID uuid.UUID, at, since time.Time, limit int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.attempts[userID][:0]
	for _, a := range f.attempts[userID] {
		if a.at.After(since) {
			kept = append(kept, a)
		}
	}
	f.attempts[userID] = kept
	if len(kept) >= limit {
		return false, nil
	}
	f.attempts[userID] = append(f.attempts[userID], fakeAttempt{id: id, at: at})
	return true, nil
}

func (f *fakeSlackLinks) ReleaseAttempt(_ context.Context, _ db.TenantScope, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for user, list := range f.attempts {
		for i, a := range list {
			if a.id == id {
				f.attempts[user] = append(list[:i:i], list[i+1:]...)
				return nil
			}
		}
	}
	return nil
}

func (f *fakeSlackLinks) RecordFact(_ context.Context, _ db.TenantScope, fact domain.SlackLinkFact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = append(f.facts, fact)
	return nil
}

// ---------------------------------------------------------------- fixture

const (
	linkTeam   = "T9TK3CUKW"
	linkMember = "U0123456789"
)

type linkFixture struct {
	svc        *service.Service
	users      *fakeUsers
	identities *fakeSlackIdentities
	links      *fakeSlackLinks
	clk        *clock.Fake
	scope      db.TenantScope
	org        uuid.UUID
	me         domain.User
	other      domain.User
	identity   domain.SlackIdentity
	logs       *bytes.Buffer
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	org := id.New()
	scope, err := db.NewTenantScope(org)
	if err != nil {
		t.Fatal(err)
	}
	f := &linkFixture{
		users:      newFakeUsers(),
		identities: &fakeSlackIdentities{byID: map[uuid.UUID]domain.SlackIdentity{}},
		links:      &fakeSlackLinks{codes: map[uuid.UUID]*fakeCode{}, attempts: map[uuid.UUID][]fakeAttempt{}},
		clk:        clock.NewFake(epoch),
		scope:      scope,
		org:        org,
		logs:       &bytes.Buffer{},
	}
	f.me = f.person(t, "ram@example.test")
	f.other = f.person(t, "ada@example.test")

	team, _ := domain.NewSlackTeamID(linkTeam)
	member, _ := domain.NewSlackUserID(linkMember)
	si, err := domain.NewSlackIdentity(id.New(), org, team, member, "ram")
	if err != nil {
		t.Fatal(err)
	}
	f.identity = si
	f.identities.put(si)

	f.svc = service.New(service.Deps{
		Users:      f.users,
		Slack:      f.identities,
		SlackLinks: f.links,
		Hasher:     cheapHasher{},
		Clock:      f.clk,
		Logger:     slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return f
}

func (f *linkFixture) person(t *testing.T, address string) domain.User {
	t.Helper()
	email, err := domain.NewEmail(address)
	if err != nil {
		t.Fatal(err)
	}
	u, err := domain.NewUser(id.New(), f.org, email, "Member", domain.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	f.users.add(u)
	return u
}

// shadow binds the identity to a shadow member, as a first Slack press does (00074).
func (f *linkFixture) shadow(t *testing.T) domain.User {
	t.Helper()
	sh, err := domain.NewShadowUser(id.New(), f.org, "@ram")
	if err != nil {
		t.Fatal(err)
	}
	f.users.add(sh)
	si, err := f.identities.get(f.identity.ID).Link(sh.ID, epoch)
	if err != nil {
		t.Fatal(err)
	}
	f.identities.put(si)
	return sh
}

func (f *linkFixture) session(u domain.User) authn.Principal {
	return authn.Principal{Kind: authn.KindSession, OrgID: f.org, UserID: u.ID, Email: u.Email.String()}
}

func (f *linkFixture) issue(t *testing.T) string {
	t.Helper()
	got, err := f.svc.IssueSlackLinkCode(context.Background(), f.scope, linkTeam, linkMember)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return got.Code
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if errs.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

// ------------------------------------------------------------------ tests

func TestAnUnlinkedMemberIsIssuedACodeThatLivesTenMinutesAndIsNeverLogged(t *testing.T) {
	f := newLinkFixture(t)
	f.shadow(t)
	got, err := f.svc.IssueSlackLinkCode(context.Background(), f.scope, linkTeam, linkMember)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Code) != 11 || got.Code[5] != '-' {
		t.Fatalf("code %q is not shown as two groups of five", got.Code)
	}
	if !got.ExpiresAt.Equal(epoch.Add(10 * time.Minute)) {
		t.Fatalf("expires %s, want ten minutes after the app clock", got.ExpiresAt)
	}
	if strings.Contains(f.logs.String(), got.Code[:5]) || strings.Contains(f.logs.String(), got.Code[6:]) {
		t.Fatalf("the code reached a log line: %s", f.logs.String())
	}
	code, _ := domain.ParseSlackLinkCode(got.Code)
	want, _ := code.Hash()
	if f.links.codes[f.identity.ID].hash != want {
		t.Fatal("the stored hash is not the sha256 of the code shown")
	}
}

func TestAMemberLinkedToARealUserIsIssuedNoCode(t *testing.T) {
	f := newLinkFixture(t)
	si, _ := f.identity.Link(f.other.ID, epoch)
	f.identities.put(si)
	_, err := f.svc.IssueSlackLinkCode(context.Background(), f.scope, linkTeam, linkMember)
	requireCode(t, err, domain.SlackIdentityLinkedElsewhereCode)
	if len(f.links.codes) != 0 {
		t.Fatal("a code was stored for a member another person holds")
	}
}

func TestANewCodeInvalidatesThePreviousOne(t *testing.T) {
	f := newLinkFixture(t)
	first := f.issue(t)
	second := f.issue(t)
	if first == second {
		t.Fatal("two issues gave one code")
	}
	_, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), first)
	requireCode(t, err, domain.SlackLinkCodeInvalidCode)
	if _, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), second); err != nil {
		t.Fatalf("the newer code: %v", err)
	}
}

func TestAPreviewNamesTheMemberAndConsumesNothing(t *testing.T) {
	f := newLinkFixture(t)
	f.shadow(t)
	code := f.issue(t)
	for range 2 {
		p, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code)
		if err != nil {
			t.Fatal(err)
		}
		if p.Identity.SlackUserID.String() != linkMember || p.Identity.TeamID.String() != linkTeam ||
			p.Identity.Handle != "ram" || p.AlreadyYours {
			t.Fatalf("preview = %+v", p)
		}
	}
	if f.identities.get(f.identity.ID).UserID == f.me.ID || len(f.links.facts) != 0 {
		t.Fatal("a preview linked")
	}
	if _, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code); err != nil {
		t.Fatalf("the code did not survive two previews: %v", err)
	}
}

func TestConfirmLinksTheSignedInUserAndAdoptsTheShadow(t *testing.T) {
	f := newLinkFixture(t)
	sh := f.shadow(t)
	code := f.issue(t)

	linked, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	if err != nil {
		t.Fatal(err)
	}
	if linked.UserID != f.me.ID || f.identities.get(f.identity.ID).UserID != f.me.ID {
		t.Fatalf("linked to %s, want the signed-in user %s", linked.UserID, f.me.ID)
	}
	if len(f.users.retired) != 1 || f.users.retired[0] != sh.ID {
		t.Fatalf("retired %v, want the shadow %s", f.users.retired, sh.ID)
	}
	if len(f.links.facts) != 1 {
		t.Fatalf("recorded %d facts, want 1", len(f.links.facts))
	}
	fact := f.links.facts[0]
	if fact.Change != domain.SlackLinkLinked || fact.UserID != f.me.ID || fact.ActorID != f.me.ID ||
		fact.DisplacedUserID != sh.ID || fact.IdentityID != f.identity.ID {
		t.Fatalf("fact = %+v", fact)
	}

	// ⭐ Single use.
	_, err = f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	requireCode(t, err, domain.SlackLinkCodeInvalidCode)
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	f := newLinkFixture(t)
	code := f.issue(t)
	f.clk.Advance(10 * time.Minute)
	_, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code)
	requireCode(t, err, domain.SlackLinkCodeInvalidCode)
	_, err = f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	requireCode(t, err, domain.SlackLinkCodeInvalidCode)
	if f.identities.get(f.identity.ID).UserID == f.me.ID {
		t.Fatal("an expired code linked")
	}
}

func TestACodeIsDeadAfterFivePresentations(t *testing.T) {
	f := newLinkFixture(t)
	code := f.issue(t)
	for i := range domain.SlackLinkCodeMaxPresentations {
		if _, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code); err != nil {
			t.Fatalf("presentation %d: %v", i+1, err)
		}
	}
	_, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	requireCode(t, err, domain.SlackLinkCodeInvalidCode)
}

func TestWrongCodesAreLimitedPerUser(t *testing.T) {
	f := newLinkFixture(t)
	code := f.issue(t)
	for i := range domain.SlackLinkWrongAttemptLimit {
		_, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), "ZZZZZ-ZZZZZ")
		requireCode(t, err, domain.SlackLinkCodeInvalidCode)
		_ = i
	}
	// The budget is spent: even the RIGHT code is refused, before it is read.
	_, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	if !errs.IsKind(err, errs.KindRateLimited) || errs.CodeOf(err) != domain.SlackLinkAttemptsExhaustedCode {
		t.Fatalf("err = %v, want a 429", err)
	}
	// Another user's budget is their own.
	if _, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.other), code); err != nil {
		t.Fatalf("another user was limited by my guesses: %v", err)
	}
	// And the window passes.
	f.clk.Advance(domain.SlackLinkWrongAttemptWindow + time.Second)
	code = f.issue(t)
	if _, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestAMalformedCodeCountsAsAWrongOne(t *testing.T) {
	f := newLinkFixture(t)
	_, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), "not a code")
	requireCode(t, err, domain.SlackLinkCodeInvalidCode)
	if n := len(f.links.attempts[f.me.ID]); n != 1 {
		t.Fatalf("counted %d attempts, want 1", n)
	}
}

func TestAMemberLinkedToAnotherRealUserIsNeverMoved(t *testing.T) {
	f := newLinkFixture(t)
	f.shadow(t)
	code := f.issue(t)
	// Between the issue and the confirm, another real person links the member.
	si, _ := f.identities.get(f.identity.ID).Link(f.other.ID, epoch)
	f.identities.put(si)

	_, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code)
	requireCode(t, err, domain.SlackIdentityLinkedElsewhereCode)
	_, err = f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	requireCode(t, err, domain.SlackIdentityLinkedElsewhereCode)
	if !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("kind = %s, want a 409", errs.KindOf(err))
	}
	if got := f.identities.get(f.identity.ID).UserID; got != f.other.ID {
		t.Fatalf("the link moved to %s", got)
	}
	if len(f.links.facts) != 0 || len(f.users.retired) != 0 {
		t.Fatal("a refused link wrote something")
	}
}

func TestAPreviewOfYourOwnLinkSaysSo(t *testing.T) {
	f := newLinkFixture(t)
	si, _ := f.identity.Link(f.me.ID, epoch)
	f.identities.put(si)
	// A member linked to me is issued no code (it would be refused) — so seed one directly.
	code, _ := domain.NewSlackLinkCode(strings.NewReader(strings.Repeat("\x01", 64)))
	h, _ := code.Hash()
	_ = f.links.IssueCode(context.Background(), f.scope, f.identity.ID, h, epoch, epoch.Add(time.Minute))
	p, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code.Display())
	if err != nil || !p.AlreadyYours {
		t.Fatalf("preview = %+v, %v", p, err)
	}
}

func TestOnlyASessionLinksOrUnlinks(t *testing.T) {
	f := newLinkFixture(t)
	code := f.issue(t)
	for name, p := range map[string]authn.Principal{
		"a PAT":            {Kind: authn.KindPAT, OrgID: f.org, UserID: f.me.ID},
		"a system caller":  {Kind: authn.KindSystem, OrgID: f.org},
		"a session nobody": {Kind: authn.KindSession, OrgID: f.org},
	} {
		if _, err := f.svc.PreviewSlackLink(context.Background(), f.scope, p, code); !errs.IsKind(err, errs.KindForbidden) {
			t.Fatalf("%s previewed: %v", name, err)
		}
		if _, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, p, code); !errs.IsKind(err, errs.KindForbidden) {
			t.Fatalf("%s confirmed: %v", name, err)
		}
		if err := f.svc.UnlinkSlackIdentity(context.Background(), f.scope, p, f.identity.ID); !errs.IsKind(err, errs.KindForbidden) {
			t.Fatalf("%s unlinked: %v", name, err)
		}
	}
}

func TestAUserUnlinksTheirOwnSlackAccountAndNobodyElses(t *testing.T) {
	f := newLinkFixture(t)
	si, _ := f.identity.Link(f.other.ID, epoch)
	f.identities.put(si)

	// Not mine: a 404, and nothing moves.
	err := f.svc.UnlinkSlackIdentity(context.Background(), f.scope, f.session(f.me), f.identity.ID)
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("err = %v, want a 404", err)
	}
	if f.identities.get(f.identity.ID).UserID != f.other.ID {
		t.Fatal("somebody else's link was dropped")
	}

	// Theirs.
	if err := f.svc.UnlinkSlackIdentity(context.Background(), f.scope, f.session(f.other), f.identity.ID); err != nil {
		t.Fatal(err)
	}
	if f.identities.get(f.identity.ID).Linked() {
		t.Fatal("the link survived its unlink")
	}
	if len(f.links.facts) != 1 || f.links.facts[0].Change != domain.SlackLinkUnlinked ||
		f.links.facts[0].ActorID != f.other.ID || f.links.facts[0].UserID != f.other.ID {
		t.Fatalf("facts = %+v", f.links.facts)
	}
	mine, err := f.svc.ListMySlackIdentities(context.Background(), f.scope, f.session(f.other))
	if err != nil || len(mine) != 0 {
		t.Fatalf("list after unlink = %v, %v", mine, err)
	}
}

func TestNoStoreFailsClosed(t *testing.T) {
	f := newLinkFixture(t)
	svc := service.New(service.Deps{Users: f.users, Slack: f.identities, Hasher: cheapHasher{}, Clock: f.clk})
	if _, err := svc.IssueSlackLinkCode(context.Background(), f.scope, linkTeam, linkMember); !errs.IsKind(err, errs.KindUnavailable) {
		t.Fatalf("issue: %v", err)
	}
	if _, err := svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), "ABCDE-FGHJK"); !errs.IsKind(err, errs.KindUnavailable) {
		t.Fatalf("confirm: %v", err)
	}
}

// ⭐ OWNER RULING F6 (2026-10-05): a Slack member linked to a DISABLED person — who cannot sign in
// to unlink it — is issued a code, and their own code moves the link, naming the disabled user as
// displaced. An ENABLED person's link still never moves (`TestAMemberLinkedToAnotherRealUserIsNeverMoved`).
func TestAMemberLinkedToADisabledPersonRelinksWithTheirOwnCode(t *testing.T) {
	f := newLinkFixture(t)
	si, _ := f.identities.get(f.identity.ID).Link(f.other.ID, epoch)
	f.identities.put(si)
	disabled := f.other
	at := epoch
	disabled.DisabledAt = &at
	f.users.add(disabled)

	code := f.issue(t)
	if _, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code); err != nil {
		t.Fatalf("preview over a disabled incumbent: %v", err)
	}
	linked, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code)
	if err != nil {
		t.Fatalf("confirm over a disabled incumbent: %v", err)
	}
	if linked.UserID != f.me.ID || f.identities.get(f.identity.ID).UserID != f.me.ID {
		t.Fatalf("linked to %s, want %s", linked.UserID, f.me.ID)
	}
	if len(f.links.facts) != 1 || f.links.facts[0].DisplacedUserID != disabled.ID {
		t.Fatalf("facts = %+v; the disabled user must be recorded as displaced", f.links.facts)
	}
	if len(f.users.retired) != 0 {
		t.Fatalf("a person was retired as if a shadow: %v", f.users.retired)
	}
}

// ⛔⛔ REVIEW E2: the wrong-code limit is a reservation, not a count. Twenty parallel wrong
// previews: exactly five are evaluated, fifteen are refused before the code is read.
func TestParallelWrongCodesAreEvaluatedOnlyUpToTheLimit(t *testing.T) {
	f := newLinkFixture(t)
	f.issue(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), "ZZZZZ-ZZZZZ")
			mu.Lock()
			got[errs.CodeOf(err)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got[domain.SlackLinkCodeInvalidCode] != domain.SlackLinkWrongAttemptLimit ||
		got[domain.SlackLinkAttemptsExhaustedCode] != 20-domain.SlackLinkWrongAttemptLimit {
		t.Fatalf("outcomes = %v, want %d evaluated and the rest refused", got, domain.SlackLinkWrongAttemptLimit)
	}
}

// A right code gives its reserved attempt back: after four wrong codes, the right one links and
// the budget still holds four.
func TestARightCodeAfterWrongOnesLinksAndLeavesTheCount(t *testing.T) {
	f := newLinkFixture(t)
	code := f.issue(t)
	for range domain.SlackLinkWrongAttemptLimit - 1 {
		_, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), "ZZZZZ-ZZZZZ")
		requireCode(t, err, domain.SlackLinkCodeInvalidCode)
	}
	if _, err := f.svc.PreviewSlackLink(context.Background(), f.scope, f.session(f.me), code); err != nil {
		t.Fatalf("preview of the right code: %v", err)
	}
	if _, err := f.svc.ConfirmSlackLink(context.Background(), f.scope, f.session(f.me), code); err != nil {
		t.Fatalf("the right code after four wrong ones: %v", err)
	}
	if n := len(f.links.attempts[f.me.ID]); n != domain.SlackLinkWrongAttemptLimit-1 {
		t.Fatalf("the budget holds %d attempts, want %d", n, domain.SlackLinkWrongAttemptLimit-1)
	}
}

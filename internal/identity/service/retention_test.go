package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/identity/domain"
	"github.com/thulasiram/oto/internal/identity/service"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
)

// MaxRetention is the reduce `partitions.manage` drops partitions on, and these
// tests pin the three properties `internal/app`'s fold cannot see from its side
// of the port: the walk reaches EVERY page, the declarative overlay is applied
// to EVERY row, and a failure mid-walk is an error rather than a partial
// maximum. The app-side tests (`internal/app/effective_retention_test.go`) pin
// what the caller does with the answer — and with the error.

// pagedOrgs is an in-memory OrgReader whose ListLive serves its rows in SMALL
// pages regardless of the limit it is asked for.
//
// ⭐ THE UNDERSIZED PAGE IS THE POINT. MaxRetention's contract is that it ends
// the walk only on an EMPTY page, precisely so a limit clamped anywhere between
// it and the SQL cannot truncate the reduce. A fake that served everything in
// one page would leave that property — the difference between an exact maximum
// and a silently narrower one — unexercised.
type pagedOrgs struct {
	orgs     []domain.Org // ascending by ID, as the keyset walk returns them
	pageSize int
	calls    int
	err      error // returned by every ListLive call when set
}

func (p *pagedOrgs) ListLive(_ context.Context, after uuid.UUID, _ int) ([]domain.Org, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	start := 0
	if after != uuid.Nil {
		for i, org := range p.orgs {
			if org.ID == after {
				start = i + 1
				break
			}
		}
	}
	end := min(start+p.pageSize, len(p.orgs))
	return p.orgs[start:end], nil
}

func (p *pagedOrgs) Get(context.Context, db.TenantScope) (domain.Org, error) {
	panic("MaxRetention must not read single orgs")
}

func (p *pagedOrgs) UpdateSettings(context.Context, db.TenantScope, domain.SettingsPatch) (domain.Org, error) {
	panic("MaxRetention must not write")
}

// retentionOrg mints one org whose retention overrides are stated in the units
// the settings screen uses. Effective settings are left for `WithDeclarative`
// to derive, exactly as rows read from Postgres arrive.
func retentionOrg(rawDays, eventMonths int) domain.Org {
	overrides := domain.SettingsPatch{}
	if rawDays > 0 {
		overrides.RawRetentionDays = intp(rawDays)
	}
	if eventMonths > 0 {
		overrides.EventRetentionMonth = intp(eventMonths)
	}
	return domain.Org{ID: id.New(), Overrides: overrides}
}

// TestMaxRetentionFoldsEveryPage puts the widest org on the LAST page, behind
// two page boundaries, so a walk that trusted a short page — or failed to
// advance its cursor — returns a maximum that missed the one org whose absence
// changes a maximum.
func TestMaxRetentionFoldsEveryPage(t *testing.T) {
	t.Parallel()

	store := &pagedOrgs{
		pageSize: 2,
		orgs: []domain.Org{
			retentionOrg(1, 1),
			retentionOrg(0, 0), // never touched the settings screen: the defaults
			retentionOrg(90, 24),
			retentionOrg(14, 2),
			retentionOrg(200, 60), // the widest, reachable only via the third page
		},
	}
	svc := service.New(service.Deps{Orgs: store})

	raw, event, err := svc.MaxRetention(context.Background())
	if err != nil {
		t.Fatalf("MaxRetention: %v", err)
	}
	if want := 200 * 24 * time.Hour; raw != want {
		t.Fatalf("raw = %v, want %v: the widest tenant lives past the page boundary, so a "+
			"walk that stopped early computed a maximum over some of the tenants — and "+
			"partitions.manage DROPS on this number", raw, want)
	}
	if want := 60 * 30 * 24 * time.Hour; event != want {
		t.Fatalf("event = %v, want %v", event, want)
	}
	// Three full-ish pages plus the empty page that is the only trusted end.
	if store.calls < 4 {
		t.Fatalf("ListLive was called %d time(s); the walk must continue until a page "+
			"comes back EMPTY, because a short page can be a clamp and a clamp must not "+
			"truncate a reduce", store.calls)
	}
}

// TestMaxRetentionAppliesTheDeclarativeOverlay is the reason this reduce lives
// in identity at all: the deployment's declarative value BEATS every org's own,
// so an org configured to 365 days on an install whose configuration forces 14
// is keeping 14 — and a maximum computed over the raw column would report 365,
// a number nobody is using.
func TestMaxRetentionAppliesTheDeclarativeOverlay(t *testing.T) {
	t.Parallel()

	decl, err := domain.NewDeclarative([]domain.DeclaredEntry{{
		Key: "raw_retention_days", ConfigKey: "tuning.raw_retention_days", Value: 14,
	}})
	if err != nil {
		t.Fatalf("declarative: %v", err)
	}

	store := &pagedOrgs{pageSize: 10, orgs: []domain.Org{retentionOrg(365, 0)}}
	svc := service.New(service.Deps{Orgs: store, Declarative: decl})

	raw, event, err := svc.MaxRetention(context.Background())
	if err != nil {
		t.Fatalf("MaxRetention: %v", err)
	}
	if want := 14 * 24 * time.Hour; raw != want {
		t.Fatalf("raw = %v, want %v: the org's 365-day override is SHADOWED by the "+
			"deployment's configuration, so folding it in reports a window no partition "+
			"is actually being kept for", raw, want)
	}
	// The unmanaged half still folds the org's effective value — here the default,
	// because the org never wrote one.
	if want := domain.DefaultEventRetention; event != want {
		t.Fatalf("event = %v, want the shipped default %v", event, want)
	}
}

// TestMaxRetentionFailsRatherThanAnswersPartially: the reduce is exact or it is
// an error. A partial maximum returned as a success would be indistinguishable
// from a narrower truth, and the caller's widening fallback only fires on the
// error.
func TestMaxRetentionFailsRatherThanAnswersPartially(t *testing.T) {
	t.Parallel()

	store := &pagedOrgs{err: errors.New("orgs.list: connection reset")}
	svc := service.New(service.Deps{Orgs: store})

	raw, event, err := svc.MaxRetention(context.Background())
	if err == nil {
		t.Fatal("a failing walk answered as a success; the caller can only widen on an error")
	}
	if raw != 0 || event != 0 {
		t.Fatalf("a failing walk returned numbers (%v, %v) beside its error; a caller that "+
			"used them would be using a partial maximum", raw, event)
	}
}

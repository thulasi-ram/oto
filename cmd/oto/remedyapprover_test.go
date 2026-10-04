package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	identitydomain "github.com/thulasiram/oto/internal/identity/domain"
	identityrepo "github.com/thulasiram/oto/internal/identity/repository"
	identityservice "github.com/thulasiram/oto/internal/identity/service"
	investigatordomain "github.com/thulasiram/oto/internal/investigator/domain"
	investigatorrepo "github.com/thulasiram/oto/internal/investigator/repository"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/test/harness"
)

// `oto grant remedy-approver` / `oto revoke remedy-approver` (ADR 0054 §4, git-bug
// 47f67c8) are the ONLY writers of the one permission oto has. What these tests pin: a
// grant counts for its holder on its ToolServer and nowhere else; each refusal names what
// was typed and writes nothing; revoking ends it; a disabled holder's grant stops
// counting; a shadow member can never hold one; deleting the ToolServer takes its grants.

type grantWorld struct {
	h      *harness.H
	scope  db.TenantScope
	userID uuid.UUID
	write  uuid.UUID
	read   uuid.UUID
	svc    *identityservice.Service
}

func newGrantWorld(t *testing.T) grantWorld {
	t.Helper()
	h := harness.New(t)
	seedUser(t, h, "acme", "operator@example.test", "correct-horse-battery-staple")

	var orgID, userID uuid.UUID
	require.NoError(t, h.Pool.QueryRow(h.Ctx,
		`SELECT org_id, id FROM users WHERE email = 'operator@example.test'`).Scan(&orgID, &userID))
	scope, err := db.NewTenantScope(orgID)
	require.NoError(t, err)

	limits, err := investigatordomain.NewCallLimits(0, 0)
	require.NoError(t, err)
	repo := investigatorrepo.NewToolServerRepository(h.Pool)
	insert := func(name, access string) uuid.UUID {
		d, err := investigatordomain.NewToolServerDraft(name, "http://"+name+".tools.test/mcp", "", access, "", limits)
		require.NoError(t, err)
		ts, err := repo.Insert(h.Ctx, scope, d, uuid.Nil, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		return ts.ID
	}

	svc := identityservice.New(identityservice.Deps{
		Orgs:            identityrepo.NewOrgRepository(h.Pool, h.Clock),
		Users:           identityrepo.NewUserRepository(h.Pool),
		RemedyApprovers: identityrepo.NewRemedyApproverRepository(h.Pool),
		Clock:           h.Clock,
	})
	return grantWorld{h: h, scope: scope, userID: userID, write: insert("k8s-write", "write"),
		read: insert("k8s", "read"), svc: svc}
}

func (w grantWorld) run(t *testing.T, verb string, args ...string) (string, error) {
	t.Helper()
	return capture(t, func() error {
		all := append([]string{"remedy-approver"}, args...)
		if verb == "grant" {
			return grantCommand(w.h.Ctx, w.h.DSN, all)
		}
		return revokeCommand(w.h.Ctx, w.h.DSN, all)
	})
}

func (w grantWorld) can(t *testing.T, toolServer, user uuid.UUID) bool {
	t.Helper()
	ok, err := w.svc.CanApproveRemedies(w.h.Ctx, w.scope, toolServer, user)
	require.NoError(t, err)
	return ok
}

func (w grantWorld) grants(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, w.h.Pool.QueryRow(w.h.Ctx, `SELECT count(*) FROM remedy_approver_grants`).Scan(&n))
	return n
}

var operatorOnWrite = []string{"--org", "acme", "--toolserver", "k8s-write", "--email", "operator@example.test"}

func TestAGrantFromTheHostShellLetsItsHolderApproveOnThatToolServerAndARevokeEndsIt(t *testing.T) {
	w := newGrantWorld(t)
	require.False(t, w.can(t, w.write, w.userID), "nobody holds a grant before one is given")

	out, err := w.run(t, "grant", operatorOnWrite...)
	require.NoError(t, err)
	require.Contains(t, out, "may now approve Remedies")
	require.True(t, w.can(t, w.write, w.userID), "the holder cannot approve on the ToolServer they were granted")
	require.False(t, w.can(t, w.read, w.userID), "a grant on one ToolServer counted on another")

	list, err := w.svc.RemedyApprovers(w.h.Ctx, w.scope, w.write)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, identitydomain.RemedyApproverGrantedByCLI, list[0].GrantedBy)

	// A second grant is refused, not ignored, and changes nothing.
	_, err = w.run(t, "grant", operatorOnWrite...)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already holds")
	require.Equal(t, 1, w.grants(t))

	out, err = w.run(t, "revoke", operatorOnWrite...)
	require.NoError(t, err)
	require.Contains(t, out, "can no longer approve")
	require.False(t, w.can(t, w.write, w.userID), "a revoked grant still counts")

	// Revoking what is not held is refused, so a typo is never mistaken for success.
	_, err = w.run(t, "revoke", operatorOnWrite...)
	require.Error(t, err)
	require.Contains(t, err.Error(), "holds no remedy-approver grant")
}

func TestAGrantIsRefusedForAnUnknownOrgToolServerOrUserAndOnAReadToolServer(t *testing.T) {
	w := newGrantWorld(t)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown org", []string{"--org", "nope", "--toolserver", "k8s-write", "--email", "operator@example.test"}, `"nope"`},
		{"unknown ToolServer", []string{"--org", "acme", "--toolserver", "gone", "--email", "operator@example.test"}, `"gone"`},
		{"unknown user", []string{"--org", "acme", "--toolserver", "k8s-write", "--email", "stranger@example.test"}, "stranger@example.test"},
		{"read ToolServer", []string{"--org", "acme", "--toolserver", "k8s", "--email", "operator@example.test"}, "write ToolServer"},
		{"malformed ToolServer", []string{"--org", "acme", "--toolserver", "K8S_X", "--email", "operator@example.test"}, "--toolserver"},
		{"missing email", []string{"--org", "acme", "--toolserver", "k8s-write"}, "--email"},
	} {
		_, err := w.run(t, "grant", c.args...)
		require.Error(t, err, c.name)
		require.Contains(t, err.Error(), c.want, c.name)
	}
	_, err := w.run(t, "grant", "--org", "acme")
	require.Error(t, err)
	_, err = capture(t, func() error { return grantCommand(w.h.Ctx, w.h.DSN, []string{"admin"}) })
	require.Error(t, err, "a grant other than remedy-approver was accepted")
	require.Zero(t, w.grants(t), "a refused grant wrote a row")
}

// TestADisabledHoldersGrantStopsCounting — and a disabled user cannot be granted one.
func TestADisabledHoldersGrantStopsCounting(t *testing.T) {
	w := newGrantWorld(t)
	_, err := w.run(t, "grant", operatorOnWrite...)
	require.NoError(t, err)

	_, err = w.h.Pool.Exec(w.h.Ctx, `UPDATE users SET disabled_at = $1 WHERE id = $2`,
		time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), w.userID)
	require.NoError(t, err)
	require.False(t, w.can(t, w.write, w.userID), "a disabled user's grant counted")
	require.Error(t, w.svc.RequireRemedyApprover(w.h.Ctx, w.scope, w.write, w.userID))

	list, err := w.svc.RemedyApprovers(w.h.Ctx, w.scope, w.write)
	require.NoError(t, err)
	require.Len(t, list, 1, "a disabled holder's grant is shown, not hidden")
	require.False(t, list[0].Counts())

	_, err = w.run(t, "revoke", operatorOnWrite...)
	require.NoError(t, err)
	_, err = w.run(t, "grant", operatorOnWrite...)
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")
}

// TestAShadowMemberCanNeverHoldAGrant — it has no address, so the CLI cannot name it; and
// a row written for one by hand does not count.
func TestAShadowMemberCanNeverHoldAGrant(t *testing.T) {
	w := newGrantWorld(t)
	shadow, err := identitydomain.NewShadowUser(uuid.New(), w.scope.OrgID(), "@ada")
	require.NoError(t, err)
	require.NoError(t, identityrepo.NewUserRepository(w.h.Pool).InsertShadow(w.h.Ctx, w.scope, shadow, time.Now()))

	_, err = w.h.Pool.Exec(w.h.Ctx, `INSERT INTO remedy_approver_grants
	  (org_id, tool_server_id, tool_server_access, user_id, granted_at, granted_by) VALUES ($1, $2, 'write', $3, $4, 'cli')`,
		w.scope.OrgID(), w.write, shadow.ID, time.Now())
	require.NoError(t, err)
	require.False(t, w.can(t, w.write, shadow.ID), "a shadow member's grant counted")
}

// TestTheSchemaRefusesWhatTheCLIRefuses — a hand-written row cannot sit on a read
// ToolServer or name another writer; and deleting a ToolServer deletes its grants.
func TestTheSchemaRefusesWhatTheCLIRefuses(t *testing.T) {
	w := newGrantWorld(t)
	ins := func(ts uuid.UUID, access, by string) error {
		_, err := w.h.Pool.Exec(w.h.Ctx, `INSERT INTO remedy_approver_grants
		  (org_id, tool_server_id, tool_server_access, user_id, granted_at, granted_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			w.scope.OrgID(), ts, access, w.userID, time.Now(), by)
		return err
	}
	require.Error(t, ins(w.read, "read", "cli"), "the schema let a grant sit on a read ToolServer")
	require.Error(t, ins(w.read, "write", "cli"), "the foreign key let a grant name a read ToolServer as write")
	require.Error(t, ins(w.write, "write", "api"), "the schema let something other than the CLI write a grant")

	_, err := w.run(t, "grant", operatorOnWrite...)
	require.NoError(t, err)
	_, err = w.h.Pool.Exec(w.h.Ctx, `DELETE FROM tool_servers WHERE id = $1`, w.write)
	require.NoError(t, err)
	require.Zero(t, w.grants(t), "deleting the ToolServer left its grants behind")
}

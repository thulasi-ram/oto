package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/app"
)

// grantKindRemedyApprover is the one thing `oto grant` and `oto revoke` can give or take
// (ADR 0054 §4). It is a positional word rather than the whole command so that the verb
// reads as English and a second grant, if one is ever earned, is a new word and not a
// new command.
const grantKindRemedyApprover = "remedy-approver"

// grantCommand is `oto grant remedy-approver`; revokeCommand is `oto revoke
// remedy-approver` (git-bug 47f67c8).
//
// ⭐ THEY ARE THE ONLY WAY A REMEDY APPROVER IS GIVEN OR TAKEN. oto has no admin role, and
// the grant must not be mintable from inside oto: double approval means two DIFFERENT
// holders, and a holder who could grant from the UI could mint an alt account and approve
// alone.
//
// ⛔ THEY ARE SUBCOMMANDS AND NOT ROUTES, for the identical reason `bootstrap` and
// `reset-password` are: running one needs a shell on the host and the database
// credentials — the same authority that could write the row by hand.
func grantCommand(ctx context.Context, dsn string, args []string) error {
	return remedyApproverCommand(ctx, dsn, "grant", args)
}

func revokeCommand(ctx context.Context, dsn string, args []string) error {
	return remedyApproverCommand(ctx, dsn, "revoke", args)
}

func remedyApproverCommand(ctx context.Context, dsn, verb string, args []string) error {
	if len(args) == 0 || args[0] != grantKindRemedyApprover {
		got := ""
		if len(args) > 0 {
			got = fmt.Sprintf(" %q", args[0])
		}
		return fmt.Errorf("%s: unknown grant%s; the only one is %q:\n  oto %s %s --org SLUG --toolserver NAME --email ADDRESS",
			verb, got, grantKindRemedyApprover, verb, grantKindRemedyApprover)
	}

	fs := flag.NewFlagSet(verb+" "+grantKindRemedyApprover, flag.ContinueOnError)
	var (
		orgSlug    = fs.String("org", "", "the org's slug, e.g. acme (required)")
		toolServer = fs.String("toolserver", "", "the write ToolServer's name (required)")
		email      = fs.String("email", "", "the user's email address in that org (required)")
	)
	fs.Usage = func() {
		what := "Lets the user approve Remedies that the named write ToolServer would carry out.\n" +
			"A disabled user's grant does not count. Granting one the user already holds is refused."
		if verb == "revoke" {
			what = "Takes the grant away; the user can no longer approve Remedies on that ToolServer.\n" +
				"Revoking a grant the user does not hold is refused, so a typo is never mistaken for success."
		}
		fmt.Fprintf(os.Stderr, `usage: oto %s %s --org SLUG --toolserver NAME --email ADDRESS

%s

There is no way to do this from inside oto, on purpose: double approval needs two
different holders, and an in-app grant would let one holder mint a second.

flags:
`, verb, grantKindRemedyApprover, what)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", verb, fs.Arg(0))
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("%s: connect: %w", verb, err)
	}
	defer pool.Close()

	req := app.RemedyApproverRequest{OrgSlug: *orgSlug, ToolServer: *toolServer, Email: *email}
	var res app.RemedyApproverResult
	if verb == "grant" {
		res, err = app.GrantRemedyApprover(ctx, pool, req, time.Now())
	} else {
		res, err = app.RevokeRemedyApprover(ctx, pool, req)
	}
	if err != nil {
		return remedyApproverRefusal(verb, req, err)
	}

	fmt.Printf("org_id         %s\n", res.OrgID)
	fmt.Printf("tool_server_id %s\n", res.ToolServerID)
	fmt.Printf("user_id        %s\n", res.UserID)
	if verb == "grant" {
		fmt.Printf("\n%s may now approve Remedies on ToolServer %s.\n", req.Email, req.ToolServer)
	} else {
		fmt.Printf("\n%s can no longer approve Remedies on ToolServer %s (granted %s).\n",
			req.Email, req.ToolServer, res.GrantedAt.Format(time.RFC3339))
	}
	return nil
}

// remedyApproverRefusal names what the operator typed in every refusal, so a script's log
// says which grant failed and why.
func remedyApproverRefusal(verb string, req app.RemedyApproverRequest, err error) error {
	switch {
	case errors.Is(err, app.ErrOrgNotFound):
		return fmt.Errorf("%s: no org with slug %q", verb, req.OrgSlug)
	case errors.Is(err, app.ErrToolServerNotFound):
		return fmt.Errorf("%s: no ToolServer named %q in org %q", verb, req.ToolServer, req.OrgSlug)
	case errors.Is(err, app.ErrApproverNotFound):
		return fmt.Errorf("%s: no user %q in org %q (a member who has never given oto an address cannot hold a grant)",
			verb, req.Email, req.OrgSlug)
	case errors.Is(err, app.ErrReadToolServer), errors.Is(err, app.ErrApproverDisabled),
		errors.Is(err, app.ErrAlreadyGranted), errors.Is(err, app.ErrNotGranted):
		return fmt.Errorf("%s: %s %s on %s: %w", verb, grantKindRemedyApprover, req.Email, req.ToolServer, err)
	}
	return fmt.Errorf("%s: %w", verb, err)
}

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
	"github.com/thulasiram/oto/internal/platform/errs"
)

// resetPasswordCommand is `oto reset-password`: the only way an existing
// user's password can change without a working session to change it with.
//
// ⭐ IT EXISTS BECAUSE THERE IS NO "FORGOT PASSWORD" FLOW. v1 has no admin
// console and every write under `/api/v1` is session- or PAT-scoped, so a user
// who is locked out has no route back in — until now the only documented
// recovery was destroying the database and re-bootstrapping.
//
// ⛔ IT IS A SUBCOMMAND AND NOT A ROUTE, for the identical reason `bootstrap`
// is one: an endpoint that rewrites somebody's password is account takeover
// with a friendly name. Running this needs a shell on the host and the
// database credentials — the same authority that could write the row by hand.
//
// The password is read from OTO_RESET_PASSWORD rather than a flag: a flag
// value lands in shell history and in `ps`, where it is readable by every other
// user on the box. Same rule as `bootstrap`, same reason.
func resetPasswordCommand(ctx context.Context, dsn string, args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	var (
		orgSlug = fs.String("org-slug", "", "the user's org, e.g. acme (required)")
		email   = fs.String("email", "", "the user's email address (required)")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: oto reset-password --org-slug SLUG --email ADDRESS

Sets a new password for an existing user and revokes every session of theirs.
There is no confirmation and no undo: the old password stops working and every
signed-in browser is signed out the moment this returns.

The password is read from OTO_RESET_PASSWORD, never from a flag: a flag value
is visible in shell history and in `+"`ps`"+`.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	password := os.Getenv("OTO_RESET_PASSWORD")
	if password == "" {
		return errors.New("reset-password: set OTO_RESET_PASSWORD (it is not a flag, so it cannot leak through ps or shell history)")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("reset-password: connect: %w", err)
	}
	defer pool.Close()

	res, err := app.ResetPassword(ctx, pool, app.ResetPasswordRequest{
		OrgSlug:  *orgSlug,
		Email:    *email,
		Password: password,
	}, time.Now())
	if err != nil {
		if errors.Is(err, app.ErrOrgNotFound) {
			return fmt.Errorf("reset-password: no org with slug %q", *orgSlug)
		}
		if errors.Is(err, errs.ErrNotFound) {
			return fmt.Errorf("reset-password: no user %q in org %q", *email, *orgSlug)
		}
		return err
	}

	fmt.Printf("org_id  %s\n", res.OrgID)
	fmt.Printf("user_id %s\n", res.UserID)
	fmt.Printf("\npassword changed; every session for this user has been revoked.\n")
	return nil
}

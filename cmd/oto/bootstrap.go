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

// bootstrapCommand is `oto bootstrap`: the documented way a fresh install
// becomes usable.
//
// ⭐ IT EXISTS BECAUSE THERE WAS NO INSTALL PATH AT ALL. v1 has no org-creation
// API and no signup, so `oto migrate` left a schema that `POST /auth/login` could
// never authenticate against — the product could be started and not used. Every
// test of it so far has seeded Postgres with hand-written SQL, which is not
// something a user will do and not something anyone should have to.
//
// ⛔ IT IS A SUBCOMMAND AND NOT A ROUTE. Creating the first org and a
// full-access token over unauthenticated HTTP is account takeover with a
// friendly name, and "it only works once" is a race rather than a control.
// Running this needs a shell on the host and the database credentials — the same
// authority that could write the rows by hand.
//
// The password is read from OTO_BOOTSTRAP_PASSWORD rather than a flag: a flag
// value lands in the shell history and in `ps`, where it is readable by every
// other user on the box.
func bootstrapCommand(ctx context.Context, dsn string, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	var (
		orgSlug     = fs.String("org-slug", "", "URL-safe tenant handle, e.g. acme (required)")
		orgName     = fs.String("org-name", "", "org display name (defaults to the slug)")
		email       = fs.String("email", "", "first user's email address (required)")
		displayName = fs.String("name", "", "first user's display name (defaults to the email)")
		tokenName   = fs.String("token-name", "bootstrap", "label for the API token this mints")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: oto bootstrap --org-slug SLUG --email ADDRESS [flags]

Creates the first org, its first user and its first API token, and prints the
token ONCE. Refuses to run if this deployment already has an org.

The password is read from OTO_BOOTSTRAP_PASSWORD, never from a flag: a flag
value is visible in shell history and in `+"`ps`"+`.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	password := os.Getenv("OTO_BOOTSTRAP_PASSWORD")
	if password == "" {
		return errors.New("bootstrap: set OTO_BOOTSTRAP_PASSWORD (it is not a flag, so it cannot leak through ps or shell history)")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("bootstrap: connect: %w", err)
	}
	defer pool.Close()

	res, err := app.Bootstrap(ctx, pool, app.BootstrapRequest{
		OrgSlug:     *orgSlug,
		OrgName:     *orgName,
		Email:       *email,
		DisplayName: *displayName,
		Password:    password,
		TokenName:   *tokenName,
	}, time.Now())
	if err != nil {
		return err
	}

	// ⛔ STDOUT, ONCE, AND NEVER THE LOG. The logger is structured, shipped and
	// retained; a bearer token in it is a bearer token in every log sink the
	// deployment has. This is the only place the secret is ever rendered.
	fmt.Printf("org_id       %s\n", res.OrgID)
	fmt.Printf("user_id      %s\n", res.UserID)
	fmt.Printf("token_prefix %s\n", res.TokenPrefix)
	fmt.Printf("\nAPI token (shown once, store it now):\n%s\n", res.Token)
	return nil
}

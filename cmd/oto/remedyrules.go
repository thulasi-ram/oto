package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thulasiram/oto/internal/app"
)

// remedyRulesCommand is `oto remedy-rules apply` and `oto remedy-rules show` (ADR 0054 §3;
// owner ruling 2026-10-05 on git-bug eb4f21b).
//
// ⭐ `apply` IS THE ONLY WAY AN ORG'S REMEDY RISK RULES ARE WRITTEN. A rule saying one lets one
// grant holder approve a Remedy alone, so writing one is the authority of granting a second
// approver — and it lives where that does, beside `oto grant remedy-approver`.
//
// ⛔ A SUBCOMMAND AND NOT A ROUTE, for `bootstrap`'s reason: running it needs a shell on the host
// and the database credentials — the same authority that could write the rows by hand. The HTTP
// API and the Settings screen only read the rules.
func remedyRulesCommand(ctx context.Context, dsn string, args []string) error {
	const usage = "  oto remedy-rules apply --org SLUG -f rules.yaml\n  oto remedy-rules show --org SLUG"
	if len(args) == 0 || (args[0] != "apply" && args[0] != "show") {
		got := ""
		if len(args) > 0 {
			got = fmt.Sprintf(" %q", args[0])
		}
		return fmt.Errorf("remedy-rules: unknown verb%s; the verbs are apply and show:\n%s", got, usage)
	}
	verb := args[0]

	fs := flag.NewFlagSet("remedy-rules "+verb, flag.ContinueOnError)
	orgSlug := fs.String("org", "", "the org's slug, e.g. acme (required)")
	var file *string
	if verb == "apply" {
		file = fs.String("f", "", "the rules file, YAML; - for stdin (required)")
	}
	fs.Usage = func() {
		fileFlag := ""
		if verb == "apply" {
			fileFlag = " -f rules.yaml"
		}
		what := "Prints the org's Remedy risk rules and risk model as the YAML that would apply them."
		if verb == "apply" {
			what = "Replaces the org's WHOLE Remedy risk rule list and its risk model with the file's, in one\n" +
				"transaction. A file that does not parse, names an unknown key, breaks a rule or names a\n" +
				"risk model the org does not have changes nothing. No Remedy already proposed is re-tiered."
		}
		fmt.Fprintf(os.Stderr, `usage: oto remedy-rules %s --org SLUG%s

%s

There is no way to write the rules from inside oto, on purpose: a rule saying one lets one
approver approve alone, which is the authority of granting a second approver.

flags:
`, verb, fileFlag, what)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("remedy-rules %s: unexpected argument %q", verb, fs.Arg(0))
	}
	if *orgSlug == "" {
		return fmt.Errorf("remedy-rules %s: --org is required", verb)
	}

	var data []byte
	if verb == "apply" {
		if *file == "" {
			return errors.New("remedy-rules apply: -f is required (the rules file, or - for stdin)")
		}
		var err error
		if *file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*file)
		}
		if err != nil {
			return fmt.Errorf("remedy-rules apply: read %s: %w", *file, err)
		}
		// ⭐ Parsed BEFORE the database is dialled: a broken file is refused, naming the
		// problem, without touching anything.
		if _, err := app.ParseRemedyRules(data); err != nil {
			return fmt.Errorf("remedy-rules apply: %s: %w", *file, err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("remedy-rules %s: connect: %w", verb, err)
	}
	defer pool.Close()

	if verb == "show" {
		res, err := app.ShowRemedyRules(ctx, pool, *orgSlug)
		if err != nil {
			return remedyRulesRefusal(verb, *orgSlug, err)
		}
		return printRemedyRulesYAML(os.Stdout, res)
	}

	res, err := app.ApplyRemedyRules(ctx, pool, *orgSlug, data, time.Now())
	if err != nil {
		return remedyRulesRefusal(verb, *orgSlug, err)
	}
	return printRemedyRulesSummary(os.Stdout, *orgSlug, res)
}

// printRemedyRulesSummary says what now stands: each rule in order with its tier, the risk
// model, and how many rules it replaced. It builds the whole summary first and writes it once,
// so a failed write is one error the command returns rather than nine it drops.
func printRemedyRulesSummary(w io.Writer, org string, res app.RemedyRulesResult) error {
	one := 0
	for _, r := range res.Rules {
		if r.Approvals == 1 {
			one++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "org_id     %s\n", res.OrgID)
	fmt.Fprintf(&b, "rules      %d (%d say one approval, %d say two); replaced %d\n",
		len(res.Rules), one, len(res.Rules)-one, res.Replaced)
	if res.RiskModel.Name != "" {
		fmt.Fprintf(&b, "risk_model %s (%s)\n", res.RiskModel.Name, res.RiskModel.Identity())
	} else {
		b.WriteString("risk_model none: the rules' answer stands\n")
	}
	for i, r := range res.Rules {
		fmt.Fprintf(&b, "  %2d. %-32s %d approval(s)\n", i+1, r.Name, r.Approvals)
	}
	if len(res.Rules) == 0 {
		fmt.Fprintf(&b, "\nOrg %s has no Remedy risk rules: every Remedy needs two approvals.\n", org)
	} else {
		fmt.Fprintf(&b, "\nApplied to org %s. The most severe matching rule wins; no match is two approvals.\n"+
			"No Remedy already proposed was re-tiered.\n", org)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func printRemedyRulesYAML(w io.Writer, res app.RemedyRulesResult) error {
	body, err := app.RemedyRulesYAML(res)
	if err != nil {
		return fmt.Errorf("remedy-rules show: %w", err)
	}
	var b strings.Builder
	if res.WrittenByLabel != "" {
		fmt.Fprintf(&b, "# Applied by %s at %s.\n", res.WrittenByLabel, res.WrittenAt.Format(time.RFC3339))
	} else {
		b.WriteString("# No rules were ever applied: every Remedy needs two approvals.\n")
	}
	b.Write(body)
	_, err = io.WriteString(w, b.String())
	return err
}

// remedyRulesRefusal names the org in every refusal, so a script's log says which apply failed.
func remedyRulesRefusal(verb, org string, err error) error {
	if errors.Is(err, app.ErrOrgNotFound) {
		return fmt.Errorf("remedy-rules %s: no org with slug %q", verb, org)
	}
	if verb == "apply" {
		return fmt.Errorf("remedy-rules apply --org %s: %w; nothing was changed", org, err)
	}
	return fmt.Errorf("remedy-rules %s --org %s: %w", verb, org, err)
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/ingestion/service"
	"github.com/thulasiram/oto/internal/platform/id"
)

// exitRefused is the process exit code for "nothing was changed, and here is
// why". It is NOT 1: an operator scripting a bulk replay has to be able to tell a
// batch that was refused for cause from a command that could not run at all, and
// `|| true` over both is how a refusal becomes a replay.
const exitRefused = 2

// errRefused is returned by replayCommand when the supersession gate refused. It
// carries no message, because the message has already been printed on stdout in
// full — main maps it onto exitRefused and prints nothing more.
var errRefused = errors.New("replay refused")

// replayCommand is `oto replay --batch <id>`: the one legal exit from `failed`.
//
// ⭐⭐ IT EXISTS BECAUSE §G.4 WAS BUILT TO BE REPLAYED AND NOTHING COULD TRIGGER
// A REPLAY. `ingest.process_batch` was enqueued from exactly one place — the
// accept transaction that first received the payload — so a parser bug cost every
// alert in every affected batch permanently, with Alertmanager's retry budget
// already spent. `ingest_batches` keeps the payload for precisely this, and
// AC-36 promises it.
//
// ⛔ IT IS A SUBCOMMAND AND NOT A ROUTE, for the same reason `bootstrap` is. It
// is an operator recovery action taken after a code fix ships, and it crosses the
// org boundary the API is scoped by: whoever fixed the parser does not know whose
// batches broke, and a batch id carries no scope. A tenant must never be able to
// re-enqueue work onto the ingest pool from outside.
func replayCommand(ctx context.Context, c *app.Container, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	var (
		batch = fs.String("batch", "", "the ingest batch id to replay (required)")
		force = fs.Bool("force", false, "replay even if alerts have moved on since the batch was received")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: oto replay --batch ID [--force]

Re-enqueues ingest.process_batch for a batch left `+"`failed`"+` or `+"`partial`"+`, after a
fix has shipped. The batch's stored payload is normalised again through the same
path the webhook took; every write underneath is idempotent, so a replay that is
safe is also a no-op for everything that already committed.

It REFUSES, changing nothing, when any alert the batch would touch has moved on
since the batch was received -- because replaying over a closed episode reopens
it and pages someone for an incident that is already over.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *batch == "" {
		fs.Usage()
		return errors.New("replay: --batch is required")
	}
	batchID, err := id.Parse(strings.TrimSpace(*batch))
	if err != nil {
		return fmt.Errorf("replay: %q is not a batch id: %w", *batch, err)
	}
	if c == nil || c.Ingestion == nil {
		return errors.New("replay: the database is unreachable, so no batch can be read")
	}

	res, err := c.Ingestion.Service.Replay(ctx, service.ReplayCommand{
		BatchID: batchID,
		Force:   *force,
	})
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}

	// ⛔ STDOUT, NOT THE LOG. A human ran this and is reading the answer; the
	// structured logger is for the process, and its sinks are somewhere else.
	if len(res.Superseded) > 0 {
		fmt.Print(refusalReport(res))
	}
	if res.Refused() {
		return errRefused
	}

	fmt.Printf("replayed: batch %s (%d alerts) is pending again; ingest.process_batch is queued.\n",
		shortID(res.BatchID.String()), res.AlertsTouched)
	return nil
}

// refusalReport is the whole of what an operator sees when a replay is refused,
// and it is written to be read at three in the morning.
//
// ⭐ IT NAMES THE COST IN BOTH DIRECTIONS. The refusal alone would leave someone
// staring at a batch of real alerts they cannot recover, so it says what --force
// would do — duplicate a customer's timeline, page for closed incidents — in
// those words rather than as "use --force to override". A warning that does not
// say what goes wrong is a warning people learn to skip.
//
// The same block is printed BEFORE a --force runs. Seeing the list you overrode
// is the point; printing it only on refusal would make the dangerous path the
// quiet one.
func refusalReport(res service.ReplayResult) string {
	var b strings.Builder

	lead := "refused"
	if res.Forced {
		// The verdict word has to change with the verdict. Printing "refused" above a
		// replay that then ran is how an operator concludes the tool is broken.
		lead = "forcing"
	}
	fmt.Fprintf(&b, "%s: batch %s has been overtaken.\n", lead, shortID(res.BatchID.String()))

	fmt.Fprintf(&b, "  received %s, %s: %q\n",
		res.ReceivedAt.UTC().Format(time.RFC3339), res.Status, res.Failure)
	fmt.Fprintf(&b, "  %d of %d alerts moved after this batch was received:\n",
		len(res.Superseded), res.AlertsTouched)

	for _, s := range res.Superseded {
		fmt.Fprintf(&b, "    %s  %s  %s, last moved %s\n",
			shortKey(s.AlertKey), s.Identity, s.State, s.MovedAt.UTC().Format("2006-01-02T15:04Z"))
	}

	b.WriteString("  Replaying would reopen occurrences that closed after this batch, and page for them.\n")
	if res.Forced {
		b.WriteString("  Replaying anyway, because --force was given.\n")
		return b.String()
	}
	b.WriteString("  Nothing was changed. Re-run with --force to replay anyway; --force can duplicate a\n")
	b.WriteString("  customer's timeline and send Slack for incidents that are already closed.\n")
	return b.String()
}

// shortID abbreviates a uuid to its first group, which is enough to recognise the
// batch you just typed and short enough to leave room for the sentence.
func shortID(s string) string {
	if i := strings.IndexByte(s, '-'); i > 0 {
		return s[:i]
	}
	return s
}

// shortKey abbreviates an `ak_…` alert key. The full key is in the result, and it
// is what a query wants; this is what a column wants.
func shortKey(k string) string {
	const keep = 7 // "ak_" plus four characters of digest
	if len(k) <= keep {
		return k
	}
	return k[:keep] + "..."
}

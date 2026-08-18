package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// batchQuery is the whole database side of this tool.
//
// ⭐ `cluster_key` COMES FROM THE JOIN, NOT FROM A LABEL. It is an axis of the
// derived key and it is resolved from the source's configuration; reading
// `payload->'groupLabels'->>'cluster'` would measure a different rule from the
// one the product runs.
//
// ⛔ `mode = 'push'` because a reconcile pass has no webhook envelope: the
// reconciler never writes a payload with `groupLabels` in it, so including those
// rows would inflate the "upstream group" count with empty partitions that
// Alertmanager never produced.
//
// The keyset is `received_at` alone and the order is ASC: this reads a WINDOW of
// history, and reading it oldest-first means a corpus that is still growing does
// not shift under the cursor.
const batchQuery = `
SELECT b.id, b.org_id, b.source_id, c.cluster_key, b.payload
  FROM ingest_batches b
  JOIN alert_sources s ON s.id = b.source_id AND s.org_id = b.org_id
  JOIN clusters      c ON c.id = s.cluster_id AND c.org_id = b.org_id
 WHERE b.mode = 'push'
   AND b.received_at >= $1
 ORDER BY b.received_at
 LIMIT $2`

func main() {
	var (
		dsn      = flag.String("dsn", "", "Postgres DSN of a real oto database. Empty reads -fixtures instead.")
		fixtures = flag.String("fixtures", "tools/groupreplay/testdata", "directory of *.json fixture batches, used when -dsn is empty")
		since    = flag.Duration("since", 7*24*time.Hour, "how far back to replay; ingest_batches retention caps this at 30 days")
		limit    = flag.Int("limit", 200000, "maximum batches to read")
		asJSON   = flag.Bool("json", false, "emit the report as JSON")
	)
	flag.Parse()

	var (
		batches []Batch
		err     error
	)
	if *dsn == "" {
		batches, err = loadFixtures(*fixtures)
	} else {
		batches, err = loadDB(context.Background(), *dsn, *since, *limit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "groupreplay:", err)
		os.Exit(1)
	}
	if len(batches) == 0 {
		fmt.Fprintln(os.Stderr, "groupreplay: no batches — nothing to say, and saying nothing is the honest output")
		os.Exit(1)
	}

	rep := Analyse(batches)
	if *asJSON {
		if err := rep.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "groupreplay:", err)
			os.Exit(1)
		}
		return
	}
	if *dsn == "" {
		_, _ = fmt.Fprintln(os.Stdout, "SOURCE: synthetic fixtures. This report is arithmetic, not evidence.")
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "SOURCE: %s, last %s\n", redactDSN(*dsn), *since)
	}
	rep.Write(os.Stdout)
}

// loadFixtures reads every `*.json` in dir. Each file is a JSON array of Batch.
func loadFixtures(dir string) ([]Batch, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Batch
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // an operator-named fixture directory
		if err != nil {
			return nil, err
		}
		var batches []Batch
		if err := json.Unmarshal(raw, &batches); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, batches...)
	}
	return out, nil
}

// loadDB reads a real corpus.
//
// It opens its own pool rather than reusing `internal/platform/db`: this is a
// read-only analysis tool run by hand against a database it does not own, and it
// must not be able to inherit a migration, a tenant scope or a job queue by
// importing the thing that has them.
func loadDB(ctx context.Context, dsn string, since time.Duration, limit int) ([]Batch, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, batchQuery, time.Now().UTC().Add(-since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Batch
	for rows.Next() {
		var b Batch
		var payload []byte
		if err := rows.Scan(&b.ID, &b.OrgID, &b.SourceID, &b.ClusterKey, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &b.Payload); err != nil {
			// A body oto stored but this tool cannot read is counted by being
			// absent; failing the whole run over one row would make the tool
			// unusable exactly on the corpora worth reading.
			continue
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// redactDSN prints where the corpus came from without printing the password.
func redactDSN(dsn string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "(unparseable dsn)"
	}
	return fmt.Sprintf("%s:%d/%s", cfg.Host, cfg.Port, cfg.Database)
}

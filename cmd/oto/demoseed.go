package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	alertsdomain "github.com/thulasiram/oto/internal/alerts/domain"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/id"
	"github.com/thulasiram/oto/internal/platform/secrets"
)

// demoSeedCommand is `oto demo-seed`: a believable fictional org, written into a
// bootstrapped deployment so that every screen has something on it.
//
// ⭐ IT EXISTS BECAUSE AN EMPTY PRODUCT CANNOT BE SHOWN, REVIEWED OR
// SCREENSHOTTED. `oto migrate` plus `oto bootstrap` leaves a deployment that
// works and renders eleven empty-state placeholders; there is no supported way
// to get an Alert with three Cases, a rule that changed between two of them and
// a dead-lettered delivery except to run a Prometheus for a fortnight. Reviewers
// were pasting hand-written SQL, which is how a fixture ends up violating an
// invariant nobody re-reads.
//
// ⛔ IT IS A SUBCOMMAND AND NOT A ROUTE, for exactly `bootstrap`'s reason: it
// writes across the org boundary the API is scoped by, and an endpoint that
// fabricates history is a forgery endpoint with a friendly name. It additionally
// REFUSES TO RUN IN PRODUCTION — `env: prod` is a hard stop, not a warning —
// because the one thing worse than a demo with no data is a production alert
// history with invented alerts in it.
//
// ⛔ AND IT IS A GO COMMAND RATHER THAN `db/seeds/demo.sql`, WHICH IS NOT A STYLE
// PREFERENCE. Four of the columns it writes are CONTENT ADDRESSES with a CHECK
// on their shape: `alerts.alert_key` is the base32hex of a framed SHA-256 over
// (org, cluster_key, canonical labels) (§C.2), `alerts.source_fingerprint` is
// Alertmanager's FNV-1a (§C.3), `rule_snapshots.rule_fingerprint` content-addresses
// a rule definition (§C.6) and `notifications.idempotency_key` hashes the subject
// and its state_version (§C.7). Postgres cannot compute any of the four, so a
// `.sql` seed would have to hard-code hashes that no longer describe their own
// rows — the exact drift `rule_snapshots_content_uniq` exists to make impossible
// — and it could not express "two weeks ago" without freezing a date into the
// fixture. Everything below goes through the SAME domain functions the ingest
// path uses, so a seeded row is a row oto could have written.
func demoSeedCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("demo-seed", flag.ContinueOnError)
	orgSlug := fs.String("org-slug", "acme", "slug of the bootstrapped org to fill")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: oto demo-seed [--org-slug SLUG]

Fills an already-bootstrapped org with a believable fictional history: clusters,
alert sources, alerts, cases, timelines, rule snapshots, channels, policies,
notifications and deliveries. Every name is invented and every timestamp is
relative to now.

Refuses to run when env is prod, and refuses to run twice.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// ⛔ THE PRODUCTION STOP IS FIRST AND HAS NO OVERRIDE FLAG. A flag that turns
	// this on in production is a flag somebody will paste out of a runbook.
	if cfg.IsProd() {
		return errors.New("demo-seed: refusing to run with env=prod; this command invents alert history")
	}

	keyring, err := secrets.NewKeyringFromBase64(cfg.Security.SecretKey)
	if err != nil {
		return fmt.Errorf("demo-seed: secret key: %w", err)
	}

	pool, err := pgxpool.New(ctx, cfg.DB.URL)
	if err != nil {
		return fmt.Errorf("demo-seed: connect: %w", err)
	}
	defer pool.Close()

	s := &demoSeeder{
		now:      time.Now().UTC().Truncate(time.Second),
		keyring:  keyring,
		clusters: map[string]uuid.UUID{},
		sources:  map[string]demoSourceRef{},
		users:    map[string]demoUserRef{},
		channels: map[string]demoChannelRef{},
		holders:  map[string]uuid.UUID{},
		counts:   map[string]int{},
	}

	err = db.Tx(ctx, pool, func(ctx context.Context) error {
		s.q = db.FromContext(ctx, pool)
		return s.run(ctx, strings.ToLower(strings.TrimSpace(*orgSlug)))
	})
	if err != nil {
		return err
	}

	fmt.Printf("seeded org %s (%s)\n\n", *orgSlug, s.orgID)
	for _, k := range sortedKeys(s.counts) {
		fmt.Printf("  %-24s %d\n", k, s.counts[k])
	}
	fmt.Print("\nOpen http://localhost:5173 and sign in as the bootstrap user.\n")
	return nil
}

// errDemoAlreadySeeded is returned when the org already has alerts.
var errDemoAlreadySeeded = errors.New(
	"demo-seed: this org already has alerts; the seed runs once (start over with `just reset`)")

// demoSourceRef is a seeded AlertSource, remembered by cluster key.
type demoSourceRef struct {
	id      uuid.UUID
	promURL string
}

// demoUserRef is a seeded user, remembered by address.
type demoUserRef struct {
	id    uuid.UUID
	label string
}

// demoChannelRef is a seeded Channel, remembered by name.
type demoChannelRef struct {
	id     uuid.UUID
	typ    string
	convID string
}

// demoPolicyRef is a seeded NotificationPolicy with its matchers compiled.
type demoPolicyRef struct {
	id       uuid.UUID
	name     string
	priority int
	matchers []demoMatcher
	reasons  map[string]bool
	channels []string
	template *uuid.UUID
}

// demoMatcher is one compiled label predicate.
type demoMatcher struct {
	name  string
	op    string
	value string
	re    *regexp.Regexp
}

// demoSeeder carries the ids the seed mints as it goes, so that a later row can
// point at an earlier one without a re-read.
type demoSeeder struct {
	q       db.Querier
	now     time.Time
	keyring *secrets.Keyring

	orgID     uuid.UUID
	clusters  map[string]uuid.UUID
	sources   map[string]demoSourceRef
	users     map[string]demoUserRef
	channels  map[string]demoChannelRef
	holders   map[string]uuid.UUID // template name → id
	policies  []demoPolicyRef
	threadSeq int
	deliverN  int
	counts    map[string]int
}

// run writes the whole world in ONE transaction. A half-seeded demo is worse
// than an empty one: it looks like a bug in the product rather than in the seed.
func (s *demoSeeder) run(ctx context.Context, slug string) error {
	// The advisory lock and the emptiness check are the same "refuse, do not
	// silently no-op" bargain `bootstrap` makes, taken inside the transaction so
	// two concurrent seeds cannot both see an empty table.
	if _, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, demoSeedLockKey); err != nil {
		return fmt.Errorf("demo-seed: lock: %w", err)
	}
	if err := s.q.QueryRow(ctx,
		`SELECT id FROM orgs WHERE slug = $1 AND deleted_at IS NULL`, slug,
	).Scan(&s.orgID); err != nil {
		return fmt.Errorf("demo-seed: no org %q — run `oto bootstrap --org-slug %s …` first: %w",
			slug, slug, err)
	}
	var existing int
	if err := s.q.QueryRow(ctx,
		`SELECT count(*) FROM alerts WHERE org_id = $1`, s.orgID).Scan(&existing); err != nil {
		return fmt.Errorf("demo-seed: count alerts: %w", err)
	}
	if existing > 0 {
		return errDemoAlreadySeeded
	}

	// `alert_events` is range-partitioned by month and the seed writes a
	// fortnight of history, which straddles a month boundary for half of every
	// year. Postgres refuses an INSERT with no partition, so ask for the ones
	// this run needs rather than hoping the maintenance job has been ahead.
	for _, t := range []time.Time{s.now.AddDate(0, 0, -35), s.now.AddDate(0, 0, -5), s.now} {
		if _, err := s.q.Exec(ctx,
			`SELECT oto_ensure_partition('alert_events', 'month', $1)`, t); err != nil {
			return fmt.Errorf("demo-seed: ensure partition: %w", err)
		}
	}

	for _, step := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"clusters", s.seedClusters},
		{"sources", s.seedSources},
		{"people", s.seedPeople},
		{"channels", s.seedChannels},
		{"templates", s.seedTemplates},
		{"policies", s.seedPolicies},
		{"alerts", s.seedAlerts},
		{"label projection", s.seedLabelNames},
		{"daily rollup", s.seedQualityDaily},
	} {
		if err := step.fn(ctx); err != nil {
			return fmt.Errorf("demo-seed: %s: %w", step.name, err)
		}
	}
	return nil
}

// demoSeedLockKey is this command's own advisory-lock key. Only it takes it.
const demoSeedLockKey int64 = 0x0704_0B00_7570_0002

func (s *demoSeeder) exec(ctx context.Context, sql string, args ...any) error {
	_, err := s.q.Exec(ctx, sql, args...)
	return err
}

func (s *demoSeeder) count(kind string, n int) { s.counts[kind] += n }

// ------------------------------------------------------------------ topology

func (s *demoSeeder) seedClusters(ctx context.Context) error {
	for _, c := range demoClusters {
		cid := id.New()
		if err := s.exec(ctx, `
INSERT INTO clusters (id, org_id, cluster_key, display_name, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
			cid, s.orgID, c.key, c.name, s.ago(30*day), s.now); err != nil {
			return err
		}
		s.clusters[c.key] = cid
	}
	s.count("clusters", len(demoClusters))
	return nil
}

// demoReconcileInterval is the poll period the seeded sources carry, and it is
// deliberately the ceiling `alert_sources_ivl_ck` allows.
//
// ⭐ THE FIXTURE'S HEALTH IS A PROJECTION THE RECONCILER OWNS, AND THIS IS THE
// ONLY HONEST WAY TO LET IT STAND. There is no `reconcile_enabled` and there must
// never be one again (ADR 0006, 00038): every source is polled, and the seeded
// upstreams are imaginary, so every pass against them fails. At the shipped 30 s
// period the whole demo went `unreachable` inside two minutes and every screen
// grew a banner saying oto could not reach anything — which was true of the
// fixture and a lie about the product. An hour is what the domain already offers
// a deployment that wants to be polled gently, and the first pass after that can
// only walk the sources DOWN one failure at a time (degraded, degraded, then
// unreachable) — never straight to the banner.
//
// ⚠️ THE SEEDED READING IS THEREFORE GOOD FOR ABOUT THREE HOURS, NOT FOR THE
// DEMO'S LIFE, and this comment used to claim the latter. Measured: a fresh seed
// reads 2 healthy / 1 degraded / 0 unreachable, and three passes — the first at
// `demoReconcileInterval`, so ~3 h — put all three on `unreachable` and raise the
// shell's permanent "oto cannot reach …" banner. `POST /sources/{id}/reconcile`
// reproduces it in three seconds.
//
// ⛔ AND THERE IS NO LONGER WINDOW TO BUY, WHICH IS WHY THE NUMBER IS THE CEILING
// AND NOT A COMPROMISE. The three ways out are all closed, in the domain rather
// than by taste:
//
//   - Exempt the source from the fan-out. That column existed
//     (`alert_sources.reconcile_enabled`), and 00038 dropped it because a source
//     oto stops polling keeps a frozen `healthy` that §B.4 goes on trusting —
//     `api.TestReconcileCannotBeSwitchedOff` now asserts the refusal.
//   - Seed it with no probe target. Same wall, one step later: `healthy` is
//     exactly the value that UNBLOCKS the reaper, so a source oto never dials
//     cannot honestly hold it, and anything else is the banner or a screen full
//     of `unknown`.
//   - Point the upstreams at something real. `just infra`'s Alertmanager would be
//     ingested straight into Acme's queue on the next pass (reconciliation is a
//     two-way repair), and an alert-free stand-in would need three containers AND
//     `security.allow_private_targets`, which is default-closed and deployment-
//     level — `netguard` refuses loopback for every tenant without it.
//
// So the fixture's health is honest when it is written and stale three hours
// later, and the recovery is `just reset && just demo`, not a switch.
const demoReconcileInterval = 3600

func (s *demoSeeder) seedSources(ctx context.Context) error {
	for _, src := range demoSources {
		sid := id.New()
		if err := s.exec(ctx, `
INSERT INTO alert_sources (id, org_id, cluster_id, name, kind, base_url, prometheus_url,
                           inject_labels, reconcile_interval_s, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'alertmanager', $5, $6, $7, $8, $9, $10)`,
			sid, s.orgID, s.clusters[src.cluster], src.name, src.baseURL, src.promURL,
			jsonBytes(map[string]string{"cluster": src.cluster}), demoReconcileInterval,
			s.ago(30*day), s.now,
		); err != nil {
			return err
		}
		h := src.health
		if err := s.exec(ctx, `
INSERT INTO source_health (source_id, org_id, status, last_push_at, last_reconcile_at,
                           last_reconcile_status, last_error, consecutive_failures,
                           am_version, send_resolved, clock_skew_ms, divergence_count,
                           warnings, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			sid, s.orgID, h.status, s.ago(h.lastPushAgo), s.ago(h.lastReconcileAgo),
			// last_error and consecutive_failures are always empty: a demo source is
			// either healthy or degraded by a warning, never by a failed probe.
			nilIfEmpty(h.lastReconcileStatus), nil, 0,
			nilIfEmpty(h.amVersion), h.sendResolved, h.clockSkewMS, h.divergenceCount,
			jsonBytes(s.warningsJSON(h.warnings)), s.now,
		); err != nil {
			return err
		}
		s.sources[src.cluster] = demoSourceRef{id: sid, promURL: src.promURL}
	}
	s.count("alert sources", len(demoSources))
	return nil
}

// seedPeople adds the two actors. They have NO password_hash and therefore no
// way to sign in: they exist to be the name on an ack and on a comment, and a
// fixture that mints logins nobody asked for is a fixture that ships a
// credential into somebody's dev cluster.
func (s *demoSeeder) seedPeople(ctx context.Context) error {
	for _, p := range demoPeople {
		uid := id.New()
		if err := s.exec(ctx, `
INSERT INTO users (id, org_id, email, display_name, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
			uid, s.orgID, p.email, p.name, s.ago(29*day), s.now); err != nil {
			return err
		}
		s.users[p.email] = demoUserRef{id: uid, label: p.name}
	}
	s.count("users", len(demoPeople))
	return nil
}

func (s *demoSeeder) seedChannels(ctx context.Context) error {
	conns := map[string]uuid.UUID{}
	for _, c := range demoConnections {
		var credID *uuid.UUID
		if c.credKnd != "" {
			sealed, version, err := s.keyring.Seal(ctx, c.credKnd, c.credVal)
			if err != nil {
				return err
			}
			cid := id.New()
			if err := s.exec(ctx, `
INSERT INTO channel_credentials (id, org_id, kind, sealed, key_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
				cid, s.orgID, c.credKnd, sealed, version, s.ago(28*day)); err != nil {
				return err
			}
			credID = &cid
		}
		connID := id.New()
		if err := s.exec(ctx, `
INSERT INTO channel_connections (id, org_id, type, name, config, credential_id,
                                 created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			connID, s.orgID, c.typ, c.name, []byte(c.config), credID,
			s.ago(28*day), s.now); err != nil {
			return err
		}
		conns[c.name] = connID
	}
	s.count("channel connections", len(demoConnections))

	for _, ch := range demoChannels {
		cid := id.New()
		if err := s.exec(ctx, `
INSERT INTO channels (id, org_id, connection_id, type, name, config, capabilities,
                      renderer, verbosity, health_status, health_error, health_checked_at,
                      created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			cid, s.orgID, conns[ch.connection], ch.typ, ch.name, []byte(ch.config), ch.caps,
			ch.renderer, ch.verbosity, ch.health, nilIfEmpty(ch.healthMsg), s.ago(4*minute),
			s.ago(28*day), s.now); err != nil {
			return err
		}
		s.channels[ch.name] = demoChannelRef{id: cid, typ: ch.typ, convID: ch.convID}
	}
	s.count("channels", len(demoChannels))
	return nil
}

func (s *demoSeeder) seedTemplates(ctx context.Context) error {
	for i, t := range demoTemplates {
		tid := id.New()
		at := s.ago(time.Duration(20-i) * day)
		if err := s.exec(ctx, `
INSERT INTO notification_templates (id, org_id, name, provider, format, source, version,
                                    enabled, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8, $9)`,
			tid, s.orgID, t.name, t.provider, t.format, t.source, t.enabled, at, at,
		); err != nil {
			return err
		}
		s.holders[t.name] = tid
	}
	s.count("notification templates", len(demoTemplates))
	return nil
}

func (s *demoSeeder) seedPolicies(ctx context.Context) error {
	for _, p := range demoPolicies {
		pid := id.New()
		var tmpl *uuid.UUID
		if p.template != "" {
			t, ok := s.holders[p.template]
			if !ok {
				return fmt.Errorf("policy %q names an unknown template %q", p.name, p.template)
			}
			tmpl = &t
		}
		throttle := p.throttle
		if throttle == "" {
			throttle = "{}"
		}
		if err := s.exec(ctx, `
INSERT INTO notification_policies (id, org_id, name, priority, enabled, matchers, reasons,
                                   channel_ids, subject_kinds, throttle, template_id,
                                   created_at, updated_at)
VALUES ($1, $2, $3, $4, TRUE, $5, $6, $7, ARRAY['case']::text[], $8, $9, $10, $11)`,
			pid, s.orgID, p.name, p.priority, []byte(p.matchers), p.reasons,
			s.channelIDs(p.channels), []byte(throttle), tmpl, s.ago(27*day), s.now,
		); err != nil {
			return err
		}
		matchers, err := compileMatchers(p.matchers)
		if err != nil {
			return fmt.Errorf("policy %q: %w", p.name, err)
		}
		s.policies = append(s.policies, demoPolicyRef{
			id: pid, name: p.name, priority: p.priority, matchers: matchers,
			reasons: setOf(p.reasons), channels: p.channels, template: tmpl,
		})
	}
	sort.Slice(s.policies, func(i, j int) bool { return s.policies[i].priority < s.policies[j].priority })
	s.count("notification policies", len(demoPolicies))
	return nil
}

func (s *demoSeeder) channelIDs(names []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		out = append(out, s.channels[n].id)
	}
	return out
}

// --------------------------------------------------------------- the signals

// caseRow is one written Case, kept so the notification and timeline passes can
// point at it without re-reading.
type caseRow struct {
	id           uuid.UUID
	seq          int
	startedAt    time.Time
	endedAt      *time.Time
	ackedAt      *time.Time
	stateVersion int
	snapshotID   uuid.UUID
	fingerprint  string
	drifted      bool
	prevFP       string
	spec         demoCase
}

// seedAlerts writes every Alert and DERIVES everything that hangs off it. The
// timeline, the notifications, the deliveries and the daily rollup are all
// computed from the same `demoCase` rows, so no screen can disagree with another
// about what happened.
func (s *demoSeeder) seedAlerts(ctx context.Context) error {
	for i := range demoAlerts {
		if err := s.seedAlert(ctx, demoAlerts[i]); err != nil {
			return fmt.Errorf("%s/%s: %w", demoAlerts[i].cluster, demoAlerts[i].alertname, err)
		}
	}
	return nil
}

func (s *demoSeeder) seedAlert(ctx context.Context, a demoAlert) error {
	labels := a.labels()
	ls, err := alertsdomain.NewLabelSet(labels)
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	clusterKey, err := alertsdomain.NewClusterKey(a.cluster)
	if err != nil {
		return fmt.Errorf("cluster key: %w", err)
	}
	alertKey := alertsdomain.ComputeAlertKey(s.orgID, clusterKey, ls, nil).String()
	fingerprint := alertsdomain.ComputeSourceFingerprint(ls).String()
	alertID := id.New()
	src := s.sources[a.cluster]

	// 1. The RuleSnapshots. Two of them on one alert is rule drift: same rule
	//    name, a different content address, and `rule_snapshots_content_uniq`
	//    keeping them apart on the fingerprint alone.
	snapshots := make([]uuid.UUID, len(a.rules))
	prints := make([]string, len(a.rules))
	for i, r := range a.rules {
		fp := alertsdomain.ComputeRuleFingerprint(r.expr, r.forS, r.keepS,
			map[string]string{"severity": a.severity}, a.annotations).String()
		sid := id.New()
		captured := s.ago(a.cases[minInt(i, len(a.cases)-1)].startedAgo)
		if err := s.exec(ctx, `
INSERT INTO rule_snapshots (id, org_id, source_id, rule_fingerprint, rule_file, rule_group,
                            rule_name, expr, for_seconds, keep_firing_for_seconds,
                            rule_labels, rule_annotations, origin, prometheus_url,
                            match_confidence, candidate_count, captured_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'prometheus_api', $13,
        'exact', 1, $14)`,
			sid, s.orgID, src.id, fp, r.file, r.group, a.alertname, r.expr, r.forS, r.keepS,
			jsonBytes(map[string]string{"severity": a.severity}), jsonBytes(a.annotations),
			src.promURL, captured,
		); err != nil {
			return fmt.Errorf("rule snapshot: %w", err)
		}
		snapshots[i] = sid
		prints[i] = fp
	}
	s.count("rule snapshots", len(a.rules))

	// 2. The Cases, oldest first, each carrying the snapshot that was live for it.
	//    COMPUTED here and WRITTEN at step 4: `alerts.current_case_id` and
	//    `alert_cases.alert_id` reference each other, so the alert row has to
	//    exist before a case can name it and the pointer back has to be a second
	//    write. Neither FK is deferrable and neither should be.
	cases := make([]caseRow, 0, len(a.cases))
	prevFP := ""
	for i, c := range a.cases {
		row := caseRow{
			id: id.New(), seq: i + 1, startedAt: s.ago(c.startedAgo),
			snapshotID: snapshots[c.rule], fingerprint: prints[c.rule],
			prevFP: prevFP, drifted: prevFP != "" && prevFP != prints[c.rule],
			spec: c, stateVersion: 1,
		}
		if c.endedAgo != 0 {
			e := s.ago(c.endedAgo)
			row.endedAt = &e
		}
		if c.ackBy != "" {
			t := row.startedAt.Add(c.ackAfter)
			row.ackedAt = &t
			row.stateVersion++
		}
		if row.endedAt != nil {
			row.stateVersion++
		}
		prevFP = prints[c.rule]
		cases = append(cases, row)
	}

	// 3. The Alert row, whose current-state columns are a PROJECTION of the cases
	//    above and are computed from them rather than declared beside them.
	last := cases[len(cases)-1]
	state := "firing"
	lastChange := last.startedAt
	lastSeen := s.ago(2 * minute)
	if last.endedAt != nil {
		state = "resolved"
		if last.spec.resolve == "timeout" {
			state = "expired"
		}
		lastChange = *last.endedAt
		lastSeen = *last.endedAt
	}
	var suppression *string
	suppressedBy := []byte(`{}`)
	if state == "firing" && last.spec.suppress != "" {
		suppression = &last.spec.suppress
		suppressedBy = jsonBytes(map[string]any{
			"silencedBy": last.spec.silencedBy, "inhibitedBy": nil, "mutedBy": nil,
		})
	}
	firstSeen := cases[0].startedAt
	if err := s.exec(ctx, `
INSERT INTO alerts (id, org_id, cluster_id, alert_key, source_fingerprint, alertname,
                    severity, namespace, service, cluster_key, labels, annotations,
                    generator_url, state, current_case_id, first_seen_at, last_seen_at,
                    last_state_change_at, total_cases, flap_score, is_flapping,
                    suppression_reason, suppressed_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
        $19, $20, $21, $22, $23, $24, $25)`,
		alertID, s.orgID, s.clusters[a.cluster], alertKey, fingerprint, a.alertname,
		a.severity, nilIfEmpty(a.namespace), nilIfEmpty(a.service), a.cluster,
		jsonBytes(labels), jsonBytes(a.annotations), a.generatorURL(src.promURL),
		state, nil, firstSeen, lastSeen, lastChange, len(cases),
		a.flapScore(), len(cases) >= 4, suppression, suppressedBy, firstSeen, s.now,
	); err != nil {
		return fmt.Errorf("alert: %w", err)
	}
	s.count("alerts", 1)

	for name, value := range labels {
		if err := s.exec(ctx, `
INSERT INTO alert_labels (org_id, alert_id, label_name, label_value) VALUES ($1, $2, $3, $4)`,
			s.orgID, alertID, name, value); err != nil {
			return fmt.Errorf("label projection: %w", err)
		}
	}

	// 4. The Cases at last, and only then the pointer from the Alert to the newest.
	for _, row := range cases {
		if err := s.writeCase(ctx, alertID, row); err != nil {
			return fmt.Errorf("case %d: %w", row.seq, err)
		}
	}
	s.count("alert cases", len(cases))
	if err := s.exec(ctx,
		`UPDATE alerts SET current_case_id = $2 WHERE id = $1`, alertID, last.id); err != nil {
		return fmt.Errorf("current case: %w", err)
	}

	// 5. Everything downstream of the cases.
	events := s.timelineFor(a, alertID, cases)
	if err := s.seedSnoozes(ctx, a, alertKey, alertID, &events); err != nil {
		return err
	}
	if err := s.seedEnrichments(ctx, a, alertID, cases, &events); err != nil {
		return err
	}
	if err := s.seedNotifications(ctx, a, labels, alertID, cases, &events); err != nil {
		return err
	}
	return s.writeEvents(ctx, events)
}

func (s *demoSeeder) writeCase(ctx context.Context, alertID uuid.UUID, c caseRow) error {
	lastObserved := s.ago(2 * minute)
	if c.endedAt != nil {
		lastObserved = *c.endedAt
	}
	state := "open"
	var resolve *string
	if c.endedAt != nil {
		state = "closed"
		r := c.spec.resolve
		resolve = &r
	}
	var ackBy *uuid.UUID
	var ackLabel, ackNote *string
	ackState := "unacked"
	if c.ackedAt != nil {
		u := s.users[c.spec.ackBy]
		ackState = "acked"
		ackBy = &u.id
		ackLabel = &u.label
		n := c.spec.ackNote
		ackNote = &n
	}
	var suppression *string
	suppressedBy := []byte(`{}`)
	if state == "open" && c.spec.suppress != "" {
		suppression = &c.spec.suppress
		suppressedBy = jsonBytes(map[string]any{
			"silencedBy": c.spec.silencedBy, "inhibitedBy": nil, "mutedBy": nil,
		})
	}
	return s.exec(ctx, `
INSERT INTO alert_cases (id, org_id, alert_id, seq, state, suppression_reason, suppressed_by,
                         started_at, ended_at, last_observed_at, source_starts_at,
                         source_ends_at, source_updated_at, resolve_reason, ack_state,
                         acked_by, acked_by_label, acked_at, ack_note, rule_snapshot_id,
                         value, state_version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
        $19, $20, $21, $22, $23, $24)`,
		c.id, s.orgID, alertID, c.seq, state, suppression, suppressedBy,
		c.startedAt, c.endedAt, lastObserved, c.startedAt, c.endedAt, lastObserved,
		resolve, ackState, ackBy, ackLabel, c.ackedAt, ackNote, c.snapshotID,
		c.spec.value, c.stateVersion, c.startedAt, lastObserved)
}

// ------------------------------------------------------------------ timeline

// demoEvent is one row of `alert_events` before it is written. They are
// collected across the whole alert and sorted once, so the timeline reads in the
// order it happened rather than in the order the seeder happened to build it.
type demoEvent struct {
	at        time.Time
	typ       string
	alertID   uuid.UUID
	caseID    *uuid.UUID
	actorKind string
	actorID   *string
	actorName *string
	summary   string
	payload   map[string]any
}

// timelineFor turns the cases into the append-only history they would have
// produced: created, opened, the snapshot binding, the drift, the ack, the
// comments, and the close.
func (s *demoSeeder) timelineFor(a demoAlert, alertID uuid.UUID, cases []caseRow) []demoEvent {
	out := make([]demoEvent, 0, 8*len(cases))
	first := cases[0]
	out = append(out, demoEvent{
		at: first.startedAt, typ: alertsdomain.EventAlertCreated.String(), alertID: alertID,
		actorKind: "ingest",
		summary:   fmt.Sprintf("%s seen for the first time in %s", a.alertname, a.cluster),
		payload:   map[string]any{"cluster": a.cluster, "severity": a.severity},
	})
	for i := range cases {
		c := cases[i]
		cid := c.id
		out = append(out, demoEvent{
			at: c.startedAt, typ: alertsdomain.EventCaseOpened.String(), alertID: alertID, caseID: &cid,
			actorKind: "ingest",
			summary:   fmt.Sprintf("Firing episode %d opened", c.seq),
			payload:   map[string]any{"seq": c.seq, "value": c.spec.value},
		})
		out = append(out, demoEvent{
			at: c.startedAt.Add(2 * time.Second), typ: alertsdomain.EventRuleSnapshotCaptured.String(),
			alertID: alertID, caseID: &cid, actorKind: "system",
			summary: fmt.Sprintf("Rule %s captured from Prometheus", a.alertname),
			payload: map[string]any{
				"snapshot_id": c.snapshotID.String(), "fingerprint": c.fingerprint,
				"origin": "prometheus_api", "match_confidence": "exact",
			},
		})
		if c.drifted {
			out = append(out, demoEvent{
				at: c.startedAt.Add(3 * time.Second), typ: alertsdomain.EventRuleDefinitionChanged.String(),
				alertID: alertID, caseID: &cid, actorKind: "system",
				summary: "The rule behind this alert changed since the previous episode",
				payload: map[string]any{
					"previous_fingerprint": c.prevFP, "fingerprint": c.fingerprint,
					"changed": []string{"expr", "for"},
				},
			})
		}
		if c.spec.suppress != "" {
			out = append(out, demoEvent{
				at: c.startedAt.Add(30 * time.Second), typ: alertsdomain.EventCaseSuppressed.String(),
				alertID: alertID, caseID: &cid, actorKind: "reconciler",
				summary: "Upstream is silencing this episode",
				payload: map[string]any{
					"suppression_reason": c.spec.suppress, "silenced_by": c.spec.silencedBy,
				},
			})
		}
		if c.ackedAt != nil {
			u := s.users[c.spec.ackBy]
			uid, label := u.id.String(), u.label
			out = append(out, demoEvent{
				at: *c.ackedAt, typ: alertsdomain.EventCaseAcknowledged.String(), alertID: alertID, caseID: &cid,
				actorKind: "user", actorID: &uid, actorName: &label,
				summary: fmt.Sprintf("%s acknowledged this episode", label),
				payload: map[string]any{"note": c.spec.ackNote},
			})
		}
		for _, cm := range c.spec.comments {
			u := s.users[cm.by]
			uid, label := u.id.String(), u.label
			out = append(out, demoEvent{
				at: c.startedAt.Add(cm.after), typ: alertsdomain.EventCommentAdded.String(), alertID: alertID,
				caseID: &cid, actorKind: "user", actorID: &uid, actorName: &label,
				summary: truncate(cm.body, 200),
				payload: map[string]any{"body": cm.body},
			})
		}
		if c.endedAt != nil {
			typ, verb := alertsdomain.EventCaseResolved.String(), "resolved upstream"
			if c.spec.resolve == "timeout" {
				typ = alertsdomain.EventCaseExpired.String()
				verb = "swept by the reaper after the source went quiet"
			}
			out = append(out, demoEvent{
				at: *c.endedAt, typ: typ, alertID: alertID, caseID: &cid,
				actorKind: "reconciler",
				summary:   fmt.Sprintf("Firing episode %d %s", c.seq, verb),
				payload: map[string]any{
					"resolve_reason": c.spec.resolve,
					"firing_seconds": int(c.endedAt.Sub(c.startedAt).Seconds()),
				},
			})
		}
	}
	return out
}

func (s *demoSeeder) writeEvents(ctx context.Context, events []demoEvent) error {
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	for _, e := range events {
		at := e.at
		if at.After(s.now) {
			at = s.now
		}
		var caseID any
		if e.caseID != nil {
			caseID = *e.caseID
		}
		if err := s.exec(ctx, `
INSERT INTO alert_events (id, org_id, alert_id, case_id, type, occurred_at, recorded_at,
                          actor_kind, actor_id, actor_label, summary, payload)
VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8, $9, $10, $11)`,
			id.New(), s.orgID, e.alertID, caseID, e.typ, at,
			e.actorKind, e.actorID, e.actorName, truncate(e.summary, 500),
			jsonBytes(e.payload)); err != nil {
			return fmt.Errorf("event %s: %w", e.typ, err)
		}
	}
	s.count("timeline events", len(events))
	return nil
}

// -------------------------------------------------------------------- snoozes

func (s *demoSeeder) seedSnoozes(
	ctx context.Context, a demoAlert, alertKey string, alertID uuid.UUID, events *[]demoEvent,
) error {
	for _, sn := range a.snoozes {
		u := s.users[sn.by]
		at := s.ago(sn.startedAgo)
		until := s.ago(sn.untilAgo)
		var endedAt *time.Time
		var endedReason *string
		if sn.endedReason != "" {
			endedAt = &until
			endedReason = &sn.endedReason
		}
		sid := id.New()
		if err := s.exec(ctx, `
INSERT INTO alert_snoozes (id, org_id, alert_id, alert_key, snoozed_at, snoozed_until,
                           snoozed_by, snoozed_by_label, note, ended_at, ended_reason,
                           created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $5)`,
			sid, s.orgID, alertID, alertKey, at, until, u.id, u.label, sn.note,
			endedAt, endedReason); err != nil {
			return fmt.Errorf("snooze: %w", err)
		}
		uid, label := u.id.String(), u.label
		*events = append(*events, demoEvent{
			at: at, typ: alertsdomain.EventAlertSnoozed.String(), alertID: alertID, actorKind: "user",
			actorID: &uid, actorName: &label,
			summary: fmt.Sprintf("%s asked oto to stay quiet about this alert", label),
			payload: map[string]any{
				"snooze_id": sid.String(), "until": until.Format(time.RFC3339),
				"note": sn.note, "duration_seconds": int(until.Sub(at).Seconds()),
			},
		})
		if endedAt != nil {
			*events = append(*events, demoEvent{
				at: *endedAt, typ: alertsdomain.EventAlertUnsnoozed.String(), alertID: alertID, actorKind: "system",
				summary: "The snooze on this alert ran out",
				payload: map[string]any{"snooze_id": sid.String(), "reason": sn.endedReason},
			})
		}
		s.count("snoozes", 1)
	}
	return nil
}

// ---------------------------------------------------------------- enrichments

// seedEnrichments writes the Enrichers' results WHERE THE READ PATH LOOKS FOR
// THEM: `GET /alerts/{id}/enrichments` reads `subject_kind='case'` keyed on the
// alert's current case while that case is open, and falls back to
// `subject_kind='alert'` once it has closed. Writing only one of the two is how a
// panel comes out empty on exactly half the fixture.
func (s *demoSeeder) seedEnrichments(
	ctx context.Context, a demoAlert, alertID uuid.UUID, cases []caseRow, events *[]demoEvent,
) error {
	if len(a.enrichers) == 0 {
		return nil
	}
	last := cases[len(cases)-1]
	subjectKind, subjectID := "case", last.id
	if last.endedAt != nil {
		subjectKind, subjectID = "alert", alertID
	}
	base := last.startedAt.Add(6 * time.Second)
	for i, name := range a.enrichers {
		payload, phase, status := s.enrichmentPayload(name, a, cases, last)
		computed := base.Add(time.Duration(i) * time.Second)
		expires := computed.Add(6 * hour)
		if err := s.exec(ctx, `
INSERT INTO enrichments (id, org_id, subject_kind, subject_id, enricher, enricher_version,
                         phase, status, payload, warnings, duration_ms, from_cache,
                         computed_at, expires_at)
VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $8, $9, $10, $11, $12, $13)`,
			id.New(), s.orgID, subjectKind, subjectID, name, phase, status,
			jsonBytes(payload), []string{}, 40+17*i, i == 3, computed, expires,
		); err != nil {
			return fmt.Errorf("enrichment %s: %w", name, err)
		}
		cid := last.id
		*events = append(*events, demoEvent{
			at: computed, typ: alertsdomain.EventEnrichmentCompleted.String(), alertID: alertID, caseID: &cid,
			actorKind: "enricher",
			summary:   fmt.Sprintf("Enricher %s finished", name),
			payload:   map[string]any{"enricher": name, "status": status, "phase": phase},
		})
		s.count("enrichments", 1)
	}
	return nil
}

func (s *demoSeeder) enrichmentPayload(
	name string, a demoAlert, cases []caseRow, last caseRow,
) (map[string]any, int, string) {
	switch name {
	case "prom.rule":
		r := a.rules[last.spec.rule]
		return map[string]any{
			"snapshot_id": last.snapshotID.String(), "fingerprint": last.fingerprint,
			"rule_name": a.alertname, "rule_group": r.group, "rule_file": r.file,
			"expr": r.expr, "for_seconds": r.forS, "keep_firing_for_seconds": r.keepS,
			"origin": "prometheus_api", "match_confidence": "exact", "candidate_count": 1,
			"available": true, "drifted": last.drifted, "new_version": last.drifted,
			"previous_fingerprint": last.prevFP,
		}, 1, "ok"
	case "alert.history":
		total := 0
		for _, c := range cases {
			if c.endedAt != nil {
				total += int(c.endedAt.Sub(c.startedAt).Seconds())
			}
		}
		mean := 0
		if len(cases) > 1 {
			mean = total / (len(cases) - 1)
		}
		return map[string]any{
			"count_24h": countStartedWithin(cases, s.now, day),
			"count_7d":  countStartedWithin(cases, s.now, 7*day),
			"count_30d": len(cases), "total_cases": len(cases),
			"flap_score": a.flapScore(), "is_flapping": len(cases) >= 4,
			"first_seen_at": cases[0].startedAt.Format(time.RFC3339),
			"last_seen_at":  last.startedAt.Format(time.RFC3339),
			"firing_duration": map[string]any{
				"samples": len(cases) - 1, "mean_s": mean, "max_s": mean,
			},
			"noisy": len(cases) >= 4,
		}, 1, "ok"
	case "runbook.link":
		links := []map[string]string{}
		for _, k := range []string{"runbook_url", "dashboard"} {
			if v, ok := a.annotations[k]; ok {
				kind := "runbook"
				if k == "dashboard" {
					kind = "dashboard"
				}
				links = append(links, map[string]string{"kind": kind, "source": k, "url": v})
			}
		}
		return map[string]any{
			"runbook": a.annotations["runbook_url"], "links": links,
		}, 1, "ok"
	default:
		return map[string]any{
			"related": []map[string]string{{
				"relation": "same_service", "alertname": a.alertname,
				"service": a.service, "namespace": a.namespace,
			}},
		}, 2, "partial"
	}
}

// ------------------------------------------------------------- notifications

// seedNotifications derives the intents to communicate and the messages that
// carried them. The mix of outcomes is DELIBERATE and not decoration: a
// notifications page on which everything succeeded never shows the dead-letter
// panel, which is the one panel that exists to prove oto's silence is never
// indistinguishable from "no alert".
func (s *demoSeeder) seedNotifications(
	ctx context.Context, a demoAlert, labels map[string]string,
	alertID uuid.UUID, cases []caseRow, events *[]demoEvent,
) error {
	policy, ok := s.matchPolicy(labels)
	if !ok {
		return nil
	}
	for _, c := range cases {
		for _, n := range s.intentsFor(a, c, policy) {
			if err := s.writeNotification(ctx, policy, alertID, c, n, events); err != nil {
				return err
			}
		}
	}
	return nil
}

// demoIntent is one Notification before it is written. `subject` is the sentence
// the message would have carried, kept here so the stored `rendered_fallback` on
// every delivery reads like a message a person received rather than like a row id.
type demoIntent struct {
	reason     string
	subject    string
	at         time.Time
	version    int
	occasion   uuid.UUID
	suppressed string
}

func (s *demoSeeder) intentsFor(a demoAlert, c caseRow, p demoPolicyRef) []demoIntent {
	var out []demoIntent
	subject := fmt.Sprintf("%s · %s", a.alertname, a.cluster)
	add := func(reason string, at time.Time, version int, occasion uuid.UUID) {
		if !p.reasons[reason] {
			return
		}
		out = append(out, demoIntent{
			reason: reason, subject: subject, at: at, version: version, occasion: occasion,
		})
	}
	// A snoozed alert still forms the intent and then declines to send it: an
	// intent that is never recorded is a silence with no explanation.
	if c.spec.suppress == "" && a.activeSnooze() {
		out = append(out, demoIntent{
			reason: "fired", subject: subject, at: c.startedAt.Add(4 * time.Second),
			version: 1, suppressed: "snoozed",
		})
	} else {
		add("fired", c.startedAt.Add(4*time.Second), 1, uuid.Nil)
	}
	if c.seq > 1 {
		add("refired", c.startedAt.Add(5*time.Second), 1, uuid.Nil)
	}
	if c.drifted {
		add("rule_changed", c.startedAt.Add(6*time.Second), 1, uuid.Nil)
	}
	if len(a.enrichers) > 0 {
		add("enriched", c.startedAt.Add(20*time.Second), 1, uuid.Nil)
	}
	if c.ackedAt != nil {
		add("acked", *c.ackedAt, 2, uuid.Nil)
	}
	for i, cm := range c.spec.comments {
		add("comment", c.startedAt.Add(cm.after), c.stateVersion, uuid.NewSHA1(
			uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/comment/%d", c.id, i))))
	}
	if c.endedAt != nil {
		reason := "all_resolved"
		if c.spec.resolve == "timeout" {
			reason = "expired"
		}
		add(reason, *c.endedAt, c.stateVersion, uuid.Nil)
	}
	return out
}

func (s *demoSeeder) writeNotification(
	ctx context.Context, p demoPolicyRef, alertID uuid.UUID, c caseRow,
	n demoIntent, events *[]demoEvent,
) error {
	nid := id.New()
	key := alertsdomain.ComputeIdempotencyKey(
		s.orgID, "case", c.id, n.reason, n.version, n.occasion).String()

	if n.suppressed != "" {
		if err := s.exec(ctx, `
INSERT INTO notifications (id, org_id, subject_kind, subject_id, alert_id, case_id, reason,
                           policy_id, state_version, idempotency_key, status,
                           suppressed_reason, conversation_kind, conversation_id,
                           created_at, updated_at)
VALUES ($1, $2, 'case', $3, $4, $3, $5, $6, $7, $8, 'suppressed', $9, 'case', $3, $10, $10)`,
			nid, s.orgID, c.id, alertID, n.reason, p.id, n.version, key, n.suppressed, n.at,
		); err != nil {
			return fmt.Errorf("notification: %w", err)
		}
		cid := c.id
		*events = append(*events, demoEvent{
			at: n.at, typ: alertsdomain.EventNotificationSuppressed.String(), alertID: alertID, caseID: &cid,
			actorKind: "notifier",
			summary:   fmt.Sprintf("Notification %q withheld: %s", n.reason, n.suppressed),
			payload:   map[string]any{"reason": n.reason, "suppressed_reason": n.suppressed},
		})
		s.count("notifications", 1)
		return nil
	}

	// ⚠️ THE INTENT IS WRITTEN BEFORE THE MESSAGES IT PRODUCES, and its status is
	// settled afterwards. `notification_deliveries.notification_id` is a
	// non-deferrable FK, so this is the only order that works — and it is also the
	// order the dispatcher runs in, for the better reason that an intent nobody
	// recorded is a silence nobody can explain.
	if err := s.exec(ctx, `
INSERT INTO notifications (id, org_id, subject_kind, subject_id, alert_id, case_id, reason,
                           policy_id, state_version, idempotency_key, status,
                           conversation_kind, conversation_id, created_at, updated_at)
VALUES ($1, $2, 'case', $3, $4, $3, $5, $6, $7, $8, 'pending', 'case', $3, $9, $10)`,
		nid, s.orgID, c.id, alertID, n.reason, p.id, n.version, key, n.at, s.now,
	); err != nil {
		return fmt.Errorf("notification: %w", err)
	}
	statuses := make([]string, 0, len(p.channels))
	for _, chName := range p.channels {
		st, err := s.writeDelivery(ctx, p, chName, nid, alertID, c, n, events)
		if err != nil {
			return err
		}
		statuses = append(statuses, st)
	}
	if err := s.exec(ctx,
		`UPDATE notifications SET status = $2 WHERE id = $1`,
		nid, rollUpStatus(statuses)); err != nil {
		return fmt.Errorf("notification status: %w", err)
	}
	cid := c.id
	*events = append(*events, demoEvent{
		at: n.at, typ: alertsdomain.EventNotificationCreated.String(), alertID: alertID, caseID: &cid,
		actorKind: "notifier",
		summary:   fmt.Sprintf("Notification %q raised by policy %q", n.reason, p.name),
		payload:   map[string]any{"reason": n.reason, "policy": p.name},
	})
	s.count("notifications", 1)
	return nil
}

// ⚠️ THE ROOT OF A THREAD IS ALWAYS `sent`, and that is a constraint rather than
// optimism: `threads_open_ck` will not let a thread be `open` without a provider
// thread id, and a provider thread id is what a successful root post RETURNS. A
// fixture with a failed root and an open thread is a row the product could not
// have written.
func (s *demoSeeder) writeDelivery(
	ctx context.Context, p demoPolicyRef, chName string, notificationID, alertID uuid.UUID,
	c caseRow, n demoIntent, events *[]demoEvent,
) (string, error) {
	ch := s.channels[chName]
	did := id.New()
	mode := "post_root"
	var threadID *uuid.UUID
	var threadSeq *int
	var convID *string

	if ch.typ == "slack" {
		tid, seq, root, err := s.thread(ctx, ch, c, did)
		if err != nil {
			return "", err
		}
		threadID, threadSeq = &tid, &seq
		conv := ch.convID
		convID = &conv
		if !root {
			mode = "thread_reply"
		}
	}

	s.deliverN++
	status := "sent"
	switch {
	case mode == "post_root":
		status = "sent"
	case s.deliverN%11 == 7:
		status = "dead"
	case s.deliverN%7 == 3:
		status = "failed"
	case s.deliverN%13 == 5:
		status = "skipped"
	}

	fallback := fmt.Sprintf("%s — %s", n.subject, reasonPhrase(n.reason))
	sum := sha256.Sum256([]byte(fallback))
	rendered := jsonBytes(map[string]any{
		"blocks": []map[string]any{
			{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": "*" + n.subject + "*"}},
			{"type": "context", "elements": []map[string]string{
				{"type": "mrkdwn", "text": reasonPhrase(n.reason)},
			}},
		},
	})
	renderedHash := hex.EncodeToString(sum[:])

	var attempts int
	var sentAt, nextAttempt *time.Time
	var providerMsg, errText, errClass *string
	switch status {
	case "sent":
		attempts = 1
		t := n.at.Add(900 * time.Millisecond)
		sentAt = &t
		m := s.providerMessageID(ch, n.at)
		providerMsg = &m
	case "failed":
		attempts = 3
		e := "slack: ratelimited (retry after 30s)"
		k := "rate_limited"
		t := s.now.Add(90 * time.Second)
		errText, errClass, nextAttempt = &e, &k, &t
	case "dead":
		attempts = 8
		e := "slack: channel_not_found — the conversation was archived"
		k := "permanent"
		errText, errClass = &e, &k
	case "skipped":
		attempts = 0
	}

	var templateID *uuid.UUID
	var templateVersion *int
	if p.template != nil {
		templateID = p.template
		v := 1
		templateVersion = &v
	}

	if err := s.exec(ctx, `
INSERT INTO notification_deliveries (id, org_id, notification_id, channel_id, thread_id,
                                     thread_seq, mode, status, attempts, next_attempt_at,
                                     rendered, rendered_hash, rendered_fallback,
                                     provider_message_id, provider_conversation_id,
                                     error, error_class, sent_at, template_id,
                                     template_version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
        $19, $20, $21, $22)`,
		did, s.orgID, notificationID, ch.id, threadID, threadSeq, mode, status, attempts,
		nextAttempt, rendered, renderedHash, fallback, providerMsg, convID, errText,
		errClass, sentAt, templateID, templateVersion, n.at, s.now,
	); err != nil {
		return "", fmt.Errorf("delivery: %w", err)
	}
	s.count("deliveries", 1)

	cid := c.id
	evType := map[string]string{
		"sent": alertsdomain.EventDeliverySent.String(), "failed": alertsdomain.EventDeliveryFailed.String(),
		"dead": alertsdomain.EventDeliveryDead.String(), "skipped": alertsdomain.EventDeliverySkipped.String(),
	}[status]
	*events = append(*events, demoEvent{
		at: n.at.Add(time.Second), typ: evType, alertID: alertID, caseID: &cid,
		actorKind: "notifier",
		summary:   fmt.Sprintf("Delivery to %s %s", chName, deliveryVerb(status)),
		payload:   map[string]any{"channel": chName, "mode": mode, "status": status},
	})
	return status, nil
}

// thread returns the channel_threads row for this (channel, case), creating it on
// first use, and reports whether the caller is the ROOT post.
func (s *demoSeeder) thread(
	ctx context.Context, ch demoChannelRef, c caseRow, deliveryID uuid.UUID,
) (uuid.UUID, int, bool, error) {
	var tid uuid.UUID
	var nextSeq int
	err := s.q.QueryRow(ctx, `
SELECT id, next_seq FROM channel_threads
 WHERE org_id = $1 AND channel_id = $2 AND subject_kind = 'case' AND subject_id = $3`,
		s.orgID, ch.id, c.id).Scan(&tid, &nextSeq)
	if err == nil {
		if _, uerr := s.q.Exec(ctx, `
UPDATE channel_threads
   SET next_seq = next_seq + 1, last_sent_seq = $2, reply_count = reply_count + 1,
       updated_at = $3
 WHERE id = $1`, tid, nextSeq, s.now); uerr != nil {
			return uuid.Nil, 0, false, fmt.Errorf("thread bump: %w", uerr)
		}
		return tid, nextSeq, false, nil
	}

	tid = id.New()
	ts := s.slackTS(c.startedAt)
	if ierr := s.exec(ctx, `
INSERT INTO channel_threads (id, org_id, channel_id, subject_kind, subject_id,
                             provider_conversation_id, provider_thread_id, root_delivery_id,
                             reply_count, last_sent_seq, next_seq, state, created_at, updated_at)
VALUES ($1, $2, $3, 'case', $4, $5, $6, $7, 0, 1, 2, 'open', $8, $9)`,
		tid, s.orgID, ch.id, c.id, ch.convID, ts, deliveryID, c.startedAt, s.now,
	); ierr != nil {
		return uuid.Nil, 0, false, fmt.Errorf("thread: %w", ierr)
	}
	s.count("channel threads", 1)
	return tid, 1, true, nil
}

// ------------------------------------------------------------- the projections

// seedLabelNames rebuilds `alert_label_names` from the rows just written. It is
// an aggregate rather than a running total for the same reason the ingest path's
// version is not: a count maintained by two writers is a count that can be
// observed disagreeing with the table it summarises.
func (s *demoSeeder) seedLabelNames(ctx context.Context) error {
	return s.exec(ctx, `
INSERT INTO alert_label_names (org_id, label_name, alert_count)
SELECT org_id, label_name, count(DISTINCT alert_id) FROM alert_labels
 WHERE org_id = $1 GROUP BY org_id, label_name
    ON CONFLICT ON CONSTRAINT alert_label_names_pk
    DO UPDATE SET alert_count = EXCLUDED.alert_count`, s.orgID)
}

// seedQualityDaily fills the daily rollup the stats screens read. Nothing else
// writes it outside the worker, so a fresh demo shows an empty chart without it.
func (s *demoSeeder) seedQualityDaily(ctx context.Context) error {
	return s.exec(ctx, `
INSERT INTO alert_quality_daily (org_id, day, cluster_key, alertname, cases, notifications,
                                 deliveries, acked_cases, auto_resolved, expired,
                                 total_firing_seconds, flap_transitions)
SELECT a.org_id,
       (c.started_at AT TIME ZONE 'UTC')::date,
       a.cluster_key,
       a.alertname,
       count(*)::int,
       coalesce(sum(n.n), 0)::int,
       coalesce(sum(n.d), 0)::int,
       count(*) FILTER (WHERE c.ack_state = 'acked')::int,
       count(*) FILTER (WHERE c.resolve_reason = 'upstream')::int,
       count(*) FILTER (WHERE c.resolve_reason = 'timeout')::int,
       coalesce(sum(EXTRACT(EPOCH FROM (coalesce(c.ended_at, now()) - c.started_at))), 0)::bigint,
       0
  FROM alert_cases c
  JOIN alerts a ON a.id = c.alert_id AND a.org_id = c.org_id
  LEFT JOIN LATERAL (
        SELECT count(*)::int AS n,
               coalesce(sum((SELECT count(*) FROM notification_deliveries d
                              WHERE d.notification_id = x.id)), 0)::int AS d
          FROM notifications x
         WHERE x.org_id = c.org_id AND x.case_id = c.id
       ) n ON TRUE
 WHERE c.org_id = $1
 GROUP BY a.org_id, 2, a.cluster_key, a.alertname
    ON CONFLICT DO NOTHING`, s.orgID)
}

// ------------------------------------------------------------------- helpers

func (s *demoSeeder) ago(d time.Duration) time.Time { return s.now.Add(-d) }

// warningsJSON renders `source_health.warnings` in the shape sources/repository
// reads back. A bare array of strings decodes to nothing and 500s the whole
// sources page, which is how a fixture breaks a screen it was written to fill.
func (s *demoSeeder) warningsJSON(in []demoWarning) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for i, w := range in {
		row := map[string]any{
			"code": w.code, "message": w.message,
			"at": s.ago(time.Duration(i+1) * minute).Format(time.RFC3339),
		}
		// `subject` is `omitempty` in the stored shape, so an absent one is
		// absent rather than an empty string the screen would have to special-case.
		if w.subject != "" {
			row["subject"] = w.subject
		}
		out = append(out, row)
	}
	return out
}

// matchPolicy is the same first-match-wins evaluation the notifier does: the
// policies are already sorted by priority, and matchers within one are ANDed.
func (s *demoSeeder) matchPolicy(labels map[string]string) (demoPolicyRef, bool) {
	for _, p := range s.policies {
		matched := true
		for _, m := range p.matchers {
			if !m.matches(labels[m.name]) {
				matched = false
				break
			}
		}
		if matched {
			return p, true
		}
	}
	return demoPolicyRef{}, false
}

// slackTS renders a Slack message timestamp, which is a FOREIGN SYSTEM'S PRIMARY
// KEY and a string: `threads_ts_ck` wants exactly 10 digits, a dot and 6 more.
func (s *demoSeeder) slackTS(at time.Time) string {
	s.threadSeq++
	return fmt.Sprintf("%010d.%06d", at.Unix(), s.threadSeq%1000000)
}

func (s *demoSeeder) providerMessageID(ch demoChannelRef, at time.Time) string {
	if ch.typ == "slack" {
		return s.slackTS(at)
	}
	s.threadSeq++
	return fmt.Sprintf("whk_%08d", s.threadSeq)
}

func (m demoMatcher) matches(value string) bool {
	switch m.op {
	case "=":
		return value == m.value
	case "!=":
		return value != m.value
	case "=~":
		return m.re.MatchString(value)
	case "!~":
		return !m.re.MatchString(value)
	default:
		return false
	}
}

func compileMatchers(raw string) ([]demoMatcher, error) {
	var wire []struct {
		Name  string `json:"name"`
		Op    string `json:"op"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	out := make([]demoMatcher, 0, len(wire))
	for _, w := range wire {
		m := demoMatcher{name: w.Name, op: w.Op, value: w.Value}
		if w.Op == "=~" || w.Op == "!~" {
			// Fully anchored, as in Alertmanager and as in notification/domain.
			re, err := regexp.Compile("^(?:" + w.Value + ")$")
			if err != nil {
				return nil, err
			}
			m.re = re
		}
		out = append(out, m)
	}
	return out, nil
}

// labels is the Alertmanager-shaped label set this Alert's identity is computed
// from. `cluster` is in it because the seeded sources inject it, which is how a
// real deployment tells two clusters apart.
func (a demoAlert) labels() map[string]string {
	out := map[string]string{
		"alertname": a.alertname,
		"severity":  a.severity,
		"cluster":   a.cluster,
		"namespace": a.namespace,
		"job":       a.job,
		"instance":  a.instance,
	}
	if a.service != "" {
		out["service"] = a.service
	}
	for k, v := range a.extra {
		out[k] = v
	}
	return out
}

func (a demoAlert) generatorURL(promURL string) string {
	return promURL + "/graph?g0.expr=" + strings.NewReplacer(
		" ", "+", "\"", "%22", "{", "%7B", "}", "%7D", "=", "%3D", "/", "%2F",
	).Replace(a.rules[0].expr) + "&g0.tab=1"
}

// flapScore is a stand-in for the detector's own arithmetic: more episodes in a
// fortnight is a noisier signal, bounded so the bar never overflows.
func (a demoAlert) flapScore() float64 {
	score := float64(len(a.cases)) * 0.18
	if score > 1 {
		return 1
	}
	return score
}

func (a demoAlert) activeSnooze() bool {
	for _, sn := range a.snoozes {
		if sn.endedReason == "" && sn.untilAgo < 0 {
			return true
		}
	}
	return false
}

func countStartedWithin(cases []caseRow, now time.Time, w time.Duration) int {
	n := 0
	for _, c := range cases {
		if now.Sub(c.startedAt) <= w {
			n++
		}
	}
	return n
}

// rollUpStatus folds the deliveries' outcomes into the Notification's own, the
// way the dispatcher does: all sent is delivered, none sent is failed, and
// anything in between is partial — which is the status that must be visible,
// because it is the one that means somebody was told and somebody was not.
func rollUpStatus(statuses []string) string {
	if len(statuses) == 0 {
		return "pending"
	}
	sent, bad := 0, 0
	for _, st := range statuses {
		switch st {
		case "sent":
			sent++
		case "failed", "dead":
			bad++
		}
	}
	switch {
	case bad == 0 && sent == len(statuses):
		return "delivered"
	case sent == 0 && bad > 0:
		return "failed"
	case bad > 0:
		return "partial"
	default:
		return "dispatched"
	}
}

// reasonPhrase is how a Reason reads to the person who received the message.
func reasonPhrase(reason string) string {
	switch reason {
	case "fired":
		return "is firing"
	case "refired":
		return "is firing again"
	case "all_resolved":
		return "resolved upstream"
	case "expired":
		return "was swept after the source went quiet"
	case "acked":
		return "was acknowledged"
	case "rule_changed":
		return "the rule behind it changed"
	case "enriched":
		return "has new context attached"
	case "comment":
		return "has a new comment"
	default:
		return reason
	}
}

func deliveryVerb(status string) string {
	switch status {
	case "sent":
		return "landed"
	case "failed":
		return "failed and will be retried"
	case "dead":
		return "was abandoned after 8 attempts"
	default:
		return "was skipped"
	}
}

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func setOf(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, v := range in {
		out[v] = true
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

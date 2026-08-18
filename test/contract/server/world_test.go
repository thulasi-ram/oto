package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/app"
	"github.com/thulasiram/oto/internal/platform/config"
	"github.com/thulasiram/oto/internal/platform/db"
	"github.com/thulasiram/oto/internal/platform/telemetry"
	"github.com/thulasiram/oto/test/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// bootstrapPassword is the first user's password. It is a constant because a
// generated one cannot be pasted into a failure report, and because
// `app.Bootstrap` refuses anything shorter than MinBootstrapPasswordBytes.
const bootstrapPassword = "correct-horse-battery-staple"

// world is one running oto: a real container, a real database, a real HTTP
// server, a real credential, and the fixture ids the probe table addresses.
type world struct {
	t   *testing.T
	srv *httptest.Server

	// pat is the personal access token `oto bootstrap` minted. It is what every
	// authenticated probe presents.
	pat string
	// cookie is the `Set-Cookie` value POST /auth/login returned, replayed
	// verbatim on the probes that need a SESSION rather than a token.
	cookie string

	orgID uuid.UUID

	// ids are the fixture identifiers the probe table substitutes into path
	// templates. They are filled by the seed and then by the probes themselves —
	// createChannel's response is what getChannel addresses.
	ids map[string]string

	// alertmanager stands in for the upstream every source points at, so that
	// `testSource` and `reconcileSource` exercise the real client against a real
	// socket rather than answering 502 from an unroutable host.
	alertmanager *harness.Alertmanager
	// webhookSink is where the one webhook channel delivers, for `testChannel`.
	webhookSink *harness.Webhook
}

// newWorld boots the whole product once for the gate.
func newWorld(t *testing.T) *world {
	t.Helper()

	h := harness.New(t)
	am := harness.NewAlertmanager(t)
	sink := harness.NewWebhook(t)

	cfg := config.Default()
	cfg.DB.URL = h.DSN
	cfg.HTTP.BaseURL = "http://oto.test"
	// The gate calls GET /metrics, which the contract declares. A deployment that
	// switches it off is a deployment where that operation does not exist.
	cfg.Telemetry.MetricsEnabled = true
	// Every fake in this test is an httptest.Server on loopback, and the SSRF
	// guard is default-closed (§C1/§C3). Without this the source and channel
	// probes would all answer 422 and the gate would be validating refusals.
	cfg.Security.AllowPrivateTargets = true
	// Jobs are ENQUEUED but not worked: `startDeliveryDrill` must enqueue to
	// answer 202, and a worker pool would make this gate about River.
	cfg.Jobs.Enabled = true

	pools, err := db.Open(h.Ctx, cfg.DB)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}

	// `GET /metrics` is mounted only when telemetry is BOTH enabled and built.
	// Without this the operation the contract declares does not exist, and the
	// gate would be reporting a 404 as though the handler were missing.
	tel, err := telemetry.Setup(h.Ctx, cfg)
	if err != nil {
		t.Fatalf("telemetry: %v", err)
	}

	boot, err := app.Bootstrap(h.Ctx, pools.General, app.BootstrapRequest{
		OrgSlug:     "acme",
		OrgName:     "Acme",
		Email:       "ops@acme.example",
		DisplayName: "Ada Lovelace",
		Password:    bootstrapPassword,
		TokenName:   "gate-g2",
	}, time.Now())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	c, err := app.New(h.Ctx, app.Options{
		Config:    cfg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pools:     pools,
		Telemetry: tel,
	})
	if err != nil {
		t.Fatalf("container: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	srv := httptest.NewServer(c.Router())
	t.Cleanup(srv.Close)

	w := &world{
		t:            t,
		srv:          srv,
		pat:          boot.Token,
		orgID:        boot.OrgID,
		ids:          map[string]string{},
		alertmanager: am,
		webhookSink:  sink,
	}
	w.seed(h, boot.OrgID)
	return w
}

// seed writes the rows the read probes address.
//
// It writes them through the harness builders — SQL, not services — for the
// reason the harness states: a fixture is not the thing under test. What IS
// under test is the bytes the read path writes back, and those are reached over
// HTTP like any other client.
func (w *world) seed(h *harness.H, orgID uuid.UUID) {
	w.t.Helper()

	org := harness.Org{ID: orgID, Slug: "acme", Name: "Acme", Scope: harness.Scope(w.t, orgID)}
	cluster := h.Cluster(org)
	source := h.SourceAt(org, cluster, w.alertmanager.URL())
	group := h.Group(org, source, cluster)
	alert := h.Alert(org, cluster)
	alertCase := h.Case(alert, group)

	w.ids["cluster"] = cluster.ID.String()
	w.ids["source"] = source.ID.String()
	w.ids["group"] = group.ID.String()
	w.ids["alert"] = alert.ID.String()
	w.ids["case"] = alertCase.ID.String()

	// The case is already a member: `h.Case` writes `group_id`, and
	// since migration 00051 that IS the membership — there is no join table left to
	// insert into. G2 is not about grouping; it needs a group with an OPEN member
	// so that the three group verbs answer 200 rather than the perfectly correct
	// 412 an empty group earns, and the SHAPE of that 200 is the whole point.
	if alertCase.GroupID != group.ID {
		w.t.Fatalf("the fixture episode is not a member of the fixture group (%s vs %s); the group "+
			"verbs below would answer 412 and the probe table would be asserting about the wrong "+
			"response", alertCase.GroupID, group.ID)
	}

	// An id that is syntactically perfect and belongs to nobody. Every probe that
	// asks for a resource this org does not own uses it, and the answer must be
	// 404 — never 403, which would confirm the row exists.
	w.ids["stranger"] = uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd").String()

	// The two upstreams the probe table points the product at. They are fixtures
	// like any other, so `{{alertmanager}}` in a request body reaches a real
	// socket speaking the real wire format.
	w.ids["alertmanager"] = w.alertmanager.URL()
	w.ids["webhook"] = w.webhookSink.URL()
}

/* -------------------------------------------------------------------------- */
/* HTTP                                                                       */
/* -------------------------------------------------------------------------- */

// authMode is which credential a probe presents.
type authMode int

const (
	// authPAT is the bootstrap personal access token — a HUMAN principal, which
	// is what the ack/comment/snooze verbs require.
	authPAT authMode = iota
	// authNone sends no credential at all.
	authNone
	// authSession replays the login cookie.
	authSession
	// authIngest presents the per-source ingest token from `ingest_token`.
	authIngest
)

// response is one exchange, kept whole so the gate can report the bytes that
// failed rather than a boolean.
type response struct {
	status    int
	mediaType string
	body      []byte
	setCookie string
}

// call performs one request against the running server.
func (w *world) call(t *testing.T, p probe) response {
	t.Helper()

	var reader io.Reader
	if p.body != nil {
		raw, err := json.Marshal(p.body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		// The fixture ids are substituted into the ENCODED body rather than into
		// the Go value, so one expansion rule covers the URL, the headers and the
		// payload. `{{name}}` cannot occur in JSON by accident: an object's first
		// byte after `{` is always a quote.
		reader = bytes.NewReader([]byte(w.expand(string(raw))))
	} else if p.rawBody != "" {
		reader = strings.NewReader(w.expand(p.rawBody))
	}

	// A deadline, not a hope: `GET /api/v1/stream` is mounted OUTSIDE the request
	// timeout deliberately and would otherwise hold this test open forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, p.method, w.srv.URL+w.target(p), reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if p.body != nil || p.rawBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch p.auth {
	case authPAT:
		req.Header.Set("Authorization", "Bearer "+w.pat)
	case authSession:
		if w.cookie == "" {
			t.Fatal("this probe needs a session and POST /auth/login has not run yet")
		}
		req.Header.Set("Cookie", w.cookie)
	case authIngest:
		req.Header.Set("Authorization", "Bearer "+w.ids["ingest_token"])
	case authNone:
	}
	for k, v := range p.header {
		req.Header.Set(k, w.expand(v))
	}

	resp, err := w.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", p.method, w.target(p), err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := response{
		status:    resp.StatusCode,
		mediaType: mediaTypeOf(resp.Header.Get("Content-Type")),
		setCookie: resp.Header.Get("Set-Cookie"),
	}
	if p.stream {
		// An SSE response never ends. Its headers are the contract's whole claim
		// about the 200, and reading the body would hang until the deadline.
		return out
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", p.method, w.target(p), err)
	}
	out.body = raw
	return out
}

// target is the concrete request target, with {placeholders} filled from the
// fixture ids.
func (w *world) target(p probe) string {
	if p.url != "" {
		return w.expand(p.url)
	}
	return w.expand(p.tmpl)
}

// expand substitutes `{{name}}` with the fixture id recorded under that name.
//
// ⛔ It FAILS on an unknown name rather than leaving the literal in place. A
// `{{newsource}}` that survives into the request path produces a 404 that looks
// like a tenancy answer, and the gate would be validating the shape of its own
// mistake.
func (w *world) expand(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[i+2 : i+j]
		v, ok := w.ids[name]
		if !ok {
			w.t.Fatalf("the probe table names the fixture %q, which nothing has recorded", name)
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+j+2:]
	}
}

// record remembers a value from a response body so a later probe can address it.
func (w *world) record(t *testing.T, key string, body []byte, path ...string) {
	t.Helper()
	v, ok := pick(body, path...)
	if !ok {
		t.Fatalf("the response carries no %s, so no later probe can address it:\n%s",
			strings.Join(path, "."), body)
	}
	w.ids[key] = v
}

// pick reads one string out of a JSON object by member path.
func pick(body []byte, path ...string) (string, bool) {
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		return "", false
	}
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[k]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case float64:
		return fmt.Sprintf("%v", v), true
	default:
		return "", false
	}
}

// mediaTypeOf strips the parameters off a Content-Type. The contract declares
// `text/event-stream`; the server writes `text/event-stream; charset=utf-8`, and
// those are the same media type.
func mediaTypeOf(header string) string {
	if i := strings.Index(header, ";"); i >= 0 {
		header = header[:i]
	}
	return strings.TrimSpace(strings.ToLower(header))
}

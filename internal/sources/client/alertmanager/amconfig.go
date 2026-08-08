package alertmanager

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/thulasiram/oto/internal/sources/domain"
)

// MaxConfigBytes caps the `config.original` YAML blob that GET /api/v2/status
// returns before oto will parse it. The blob is untrusted input of unbounded
// size and a YAML parser is a poor place to meet a 40 MiB string.
const MaxConfigBytes = 1 << 20

// DefaultResolveTimeout is Alertmanager's `global.resolve_timeout` default.
//
// It is worth stating plainly what this value does NOT do: for alerts that came
// from Prometheus it is IRRELEVANT. Prometheus always sets EndsAt (a lease of
// now + 4 * max(interval, resend_delay)), and Alertmanager only applies
// resolve_timeout when EndsAt is absent — i.e. only for third parties POSTing
// straight to /api/v2/alerts. oto records it for the sources screen and must
// never do arithmetic with it (research A5).
const DefaultResolveTimeout = 5 * time.Minute

// ParsedConfig is what oto extracts from `config.original`.
type ParsedConfig struct {
	// ResolveTimeout is global.resolve_timeout, defaulted.
	ResolveTimeout time.Duration
	// SendResolved maps receiver name to its EFFECTIVE send_resolved: true when
	// any of the receiver's integrations will emit a resolved notification.
	SendResolved map[string]bool
	// Receivers is every receiver name in declaration order.
	Receivers []string
	// Timings are the top-level route's group_wait, group_interval and
	// repeat_interval, as the source itself reports them. See
	// `sources/domain.RouteTimings` for why every field is a pointer.
	Timings domain.RouteTimings
}

// sendResolvedDefaults are Alertmanager's per-integration defaults for
// `send_resolved` (config/notifiers.go: NotifierConfig.VSendResolved). Email is
// the odd one out at false; every other shipped integration defaults to true.
//
// An integration key absent from this map defaults to TRUE, which is the safe
// direction: a false negative here would raise a spurious C15 warning telling an
// operator their alerts will never resolve when in fact they will.
var sendResolvedDefaults = map[string]bool{
	"email_configs": false,
}

// parseConfig reads the subset of alertmanager.yml that oto needs. It is
// deliberately forgiving: the config is a moving target across releases, and
// failing a whole status probe because a future field appeared would be worse
// than reporting "unknown" for send_resolved.
func parseConfig(original string) (ParsedConfig, error) {
	out := ParsedConfig{ResolveTimeout: DefaultResolveTimeout}
	if strings.TrimSpace(original) == "" {
		return out, errors.New("alertmanager returned an empty config")
	}
	if len(original) > MaxConfigBytes {
		return out, errors.New("alertmanager config exceeds " + strconv.Itoa(MaxConfigBytes) + " bytes")
	}

	var doc struct {
		Global    map[string]any   `yaml:"global"`
		Route     map[string]any   `yaml:"route"`
		Receivers []map[string]any `yaml:"receivers"`
	}
	if err := yaml.Unmarshal([]byte(original), &doc); err != nil {
		return out, err
	}

	if raw, ok := doc.Global["resolve_timeout"]; ok {
		if d, err := ParsePromDuration(asString(raw)); err == nil && d > 0 {
			out.ResolveTimeout = d
		}
	}

	out.Timings = routeTimings(doc.Route)

	out.SendResolved = make(map[string]bool, len(doc.Receivers))
	for _, r := range doc.Receivers {
		name := asString(r["name"])
		if name == "" {
			continue
		}
		out.Receivers = append(out.Receivers, name)
		out.SendResolved[name] = receiverSendsResolved(r)
	}
	return out, nil
}

// timingFields are the three per-route batching settings oto reads, spelled as
// Alertmanager spells them.
var timingFields = []string{"group_wait", "group_interval", "repeat_interval"}

// routeTimings reads the top-level route's three timings and counts how many
// descendants set their own.
//
// ⛔ IT READS THE TOP-LEVEL ROUTE, AND THAT IS A DELIBERATE v1 BOUNDARY.
// Resolving the value that governs a PARTICULAR alert would mean evaluating
// Alertmanager's matcher tree — regex matchers, `continue: true`, mute time
// intervals and inheritance — against that alert's labels, which is a second
// implementation of somebody else's routing engine and would be wrong in a way
// nobody could see. The top-level route is the value in force for every alert
// that matches no more specific route, it is exactly what the tuning guide tells
// an operator to read, and `ChildrenWithTimings` says out loud when it is not the
// whole picture.
func routeTimings(route map[string]any) domain.RouteTimings {
	var out domain.RouteTimings
	if len(route) == 0 {
		return out
	}

	if d, ok := promDurationField(route, "group_wait"); ok {
		out.GroupWait = &d
	}
	if d, ok := promDurationField(route, "group_interval"); ok {
		out.GroupInterval = &d
	}
	if d, ok := promDurationField(route, "repeat_interval"); ok {
		out.RepeatInterval = &d
	}

	walkChildRoutes(route, &out)
	return out
}

// walkChildRoutes counts the descendants of one route and how many of them state
// a timing of their own. It recurses to the whole tree, because a timing three
// levels down still governs whatever matches it.
func walkChildRoutes(route map[string]any, out *domain.RouteTimings) {
	kids, ok := route["routes"].([]any)
	if !ok {
		return
	}
	for _, item := range kids {
		child, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out.ChildRoutes++
		for _, f := range timingFields {
			if _, has := promDurationField(child, f); has {
				out.ChildrenWithTimings++
				break
			}
		}
		walkChildRoutes(child, out)
	}
}

// promDurationField reads one Prometheus duration off a route.
//
// An ABSENT key and an UNPARSEABLE value both report false, which is the same
// answer — oto does not know — and neither is allowed to become a number. A zero
// duration, however, is a real setting (`group_wait: 0s` means "notify at once")
// and is reported as observed, which is why the test is `ok` rather than `d > 0`.
func promDurationField(route map[string]any, field string) (time.Duration, bool) {
	raw, present := route[field]
	if !present {
		return 0, false
	}
	d, err := ParsePromDuration(asString(raw))
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

// receiverSendsResolved reports the effective send_resolved for one receiver:
// true when ANY of its integrations will emit a resolved notification. A
// receiver with no integrations at all sends nothing, resolved included.
func receiverSendsResolved(receiver map[string]any) bool {
	sends := false
	for key, val := range receiver {
		if !strings.HasSuffix(key, "_configs") {
			continue
		}
		list, ok := val.([]any)
		if !ok {
			continue
		}
		def, known := sendResolvedDefaults[key]
		if !known {
			def = true
		}
		for _, item := range list {
			cfg, ok := item.(map[string]any)
			if !ok {
				// A malformed integration entry: assume the default rather than
				// silently claiming the receiver is mute.
				sends = sends || def
				continue
			}
			if raw, present := cfg["send_resolved"]; present {
				if b, ok := raw.(bool); ok {
					sends = sends || b
					continue
				}
			}
			sends = sends || def
		}
	}
	return sends
}

// asString renders a scalar YAML value as a string without reflection surprises.
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// ParsePromDuration parses a Prometheus `model.Duration` string.
//
// It is NOT time.ParseDuration: Prometheus accepts y, w and d units that the
// stdlib rejects, and rejects the fractional and negative forms the stdlib
// accepts. Getting this wrong turns `resolve_timeout: 1d` into a parse error and
// silently substitutes 5m.
func ParsePromDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if s == "0" {
		return 0, nil
	}

	units := []struct {
		suffix string
		unit   time.Duration
	}{
		{"ms", time.Millisecond},
		{"s", time.Second},
		{"m", time.Minute},
		{"h", time.Hour},
		{"d", 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
		{"y", 365 * 24 * time.Hour},
	}

	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, errors.New("not a prometheus duration: " + s)
		}
		n, err := strconv.ParseInt(rest[:i], 10, 64)
		if err != nil {
			return 0, err
		}
		rest = rest[i:]

		matched := false
		// "ms" must be tried before "m", so the table is scanned longest-first.
		for _, u := range units {
			if strings.HasPrefix(rest, u.suffix) &&
				(len(u.suffix) == 2 || !strings.HasPrefix(rest, "ms")) {
				total += time.Duration(n) * u.unit
				rest = rest[len(u.suffix):]
				matched = true
				break
			}
		}
		if !matched {
			return 0, errors.New("not a prometheus duration: " + s)
		}
	}
	return total, nil
}

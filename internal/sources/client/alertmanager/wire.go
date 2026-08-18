package alertmanager

import (
	"encoding/json"
	"time"

	"github.com/thulasiram/oto/internal/sources/domain"
)

// The wire models below are HAND-WRITTEN from the verified API v2 shapes
// (docs/design/domain-research.md A4). Alertmanager's own Go module is
// deliberately not imported: it drags in go-swagger, go-openapi/strfmt and a
// large slice of Prometheus, all to describe six flat structs, and it would pin
// oto's build to one Alertmanager release.
//
// Every field is optional at the type level even where the OpenAPI document
// marks it required, because an upstream is untrusted in SHAPE as well as in
// content (SPEC §L.3.1): decoding is lenient, unknown fields are accepted, and a
// missing field becomes a zero value rather than an error.

// wireAlert is `gettableAlert`.
type wireAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     *time.Time        `json:"startsAt"`
	EndsAt       *time.Time        `json:"endsAt"`
	UpdatedAt    *time.Time        `json:"updatedAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	// Receivers is `[]receiver` where receiver is `{"name": "..."}`. Some
	// Alertmanager-compatible servers (and older Grafana builds) emit a plain
	// array of strings instead, so the decode tolerates both.
	Receivers json.RawMessage `json:"receivers"`
	Status    wireAlertStatus `json:"status"`
}

// wireAlertStatus is `alertStatus`. Its State is the ONLY source of suppression
// truth in the whole system (C1): Alertmanager's MuteStage drops suppressed
// alerts before they can reach a webhook, so nothing on the push path can ever
// produce this.
type wireAlertStatus struct {
	State       string   `json:"state"`
	SilencedBy  []string `json:"silencedBy"`
	InhibitedBy []string `json:"inhibitedBy"`
	MutedBy     []string `json:"mutedBy"`
}

// wireReceiver is `receiver`.
type wireReceiver struct {
	Name string `json:"name"`
}

// wireSilence is `gettableSilence`.
type wireSilence struct {
	ID          string            `json:"id"`
	Status      wireSilenceStatus `json:"status"`
	UpdatedAt   *time.Time        `json:"updatedAt"`
	Comment     string            `json:"comment"`
	CreatedBy   string            `json:"createdBy"`
	StartsAt    *time.Time        `json:"startsAt"`
	EndsAt      *time.Time        `json:"endsAt"`
	Matchers    []wireMatcher     `json:"matchers"`
	Annotations map[string]string `json:"annotations"`
}

// wireSilenceStatus is `silenceStatus`; State is expired | active | pending.
type wireSilenceStatus struct {
	State string `json:"state"`
}

// wireMatcher is `matcher`. isEqual defaults to TRUE when absent, which is why
// it is a pointer: a missing isEqual and an explicit `false` mean opposite
// things, and collapsing them turns `=` into `!=`.
type wireMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual"`
}

// wireStatus is `alertmanagerStatus`.
type wireStatus struct {
	Cluster     wireClusterStatus `json:"cluster"`
	VersionInfo wireVersionInfo   `json:"versionInfo"`
	Config      wireConfig        `json:"config"`
	Uptime      *time.Time        `json:"uptime"`
}

type wireClusterStatus struct {
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Peers  []wireClusterPr `json:"peers"`
}

type wireClusterPr struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type wireVersionInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// wireConfig is `alertmanagerConfig`: the running configuration, as YAML, in one
// string. It is the only place send_resolved and resolve_timeout are exposed.
type wireConfig struct {
	Original string `json:"original"`
}

// deref returns the time a pointer holds, or the zero time. The zero time is
// meaningful on this wire — Alertmanager sends "0001-01-01T00:00:00Z" for an
// unknown endsAt rather than null or an absent key (SPEC §L.3.1).
func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// receiverNames decodes the two shapes `receivers` is seen in.
func receiverNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var objs []wireReceiver
	if err := json.Unmarshal(raw, &objs); err == nil {
		out := make([]string, 0, len(objs))
		for _, r := range objs {
			if r.Name != "" {
				out = append(out, r.Name)
			}
		}
		return out
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		return strs
	}
	return nil
}

// toAlert maps the wire model onto the port model. It copies maps so that no
// caller can mutate the decoder's memory, and it never invents a value: an
// absent timestamp stays zero.
func (a wireAlert) toAlert() domain.GettableAlert {
	return domain.GettableAlert{
		Fingerprint:  a.Fingerprint,
		Labels:       copyMap(a.Labels),
		Annotations:  copyMap(a.Annotations),
		StartsAt:     deref(a.StartsAt),
		EndsAt:       deref(a.EndsAt),
		UpdatedAt:    deref(a.UpdatedAt),
		GeneratorURL: a.GeneratorURL,
		Receivers:    receiverNames(a.Receivers),
		Status: domain.AlertStatus{
			State:       a.Status.State,
			SilencedBy:  append([]string(nil), a.Status.SilencedBy...),
			InhibitedBy: append([]string(nil), a.Status.InhibitedBy...),
			MutedBy:     append([]string(nil), a.Status.MutedBy...),
		},
	}
}

// toSilence maps `gettableSilence` onto the port model.
func (s wireSilence) toSilence() domain.GettableSilence {
	ms := make([]domain.Matcher, 0, len(s.Matchers))
	for _, m := range s.Matchers {
		isEqual := true
		if m.IsEqual != nil {
			isEqual = *m.IsEqual
		}
		ms = append(ms, domain.Matcher{
			Name:    m.Name,
			Value:   m.Value,
			IsRegex: m.IsRegex,
			IsEqual: isEqual,
		})
	}
	return domain.GettableSilence{
		ID:          s.ID,
		Matchers:    ms,
		StartsAt:    deref(s.StartsAt),
		EndsAt:      deref(s.EndsAt),
		UpdatedAt:   deref(s.UpdatedAt),
		CreatedBy:   s.CreatedBy,
		Comment:     s.Comment,
		Annotations: copyMap(s.Annotations),
		State:       s.Status.State,
	}
}

// copyMap returns a defensive copy, or nil for an empty input.
func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

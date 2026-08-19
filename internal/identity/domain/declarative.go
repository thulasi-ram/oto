package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// The DECLARATIVE layer: tuning stated by the deployment rather than by a tenant.
//
// ⭐⭐ THE PRECEDENCE, AND WHY IT IS THIS WAY ROUND:
//
//	shipped default  →  org override (Postgres)  →  declarative config  (highest)
//
// Declarative wins because that is what an operator running this under IaC
// already believes. If `refire_grace_s` is in the Helm values file, the values
// file is the truth; anything else means the cluster drifts from the repository
// that is supposed to describe it. The failure this ordering removes is the
// silent one: somebody edits the number in the UI, the next deploy reverts it,
// and nothing anywhere ever said the deployment had an opinion.
//
// ⛔ A DECLARATIVELY-MANAGED KEY IS READ-ONLY OVER THE API, AND THE REFUSAL IS A
// 409 THAT NAMES THE CONFIG KEY. Accepting the write and quietly not applying it
// would be the worst of the three options — it manufactures exactly the "it
// changed back on its own" mystery this layer exists to end.
//
// ⭐ A SHADOWED OVERRIDE IS STILL STORED AND STILL VISIBLE. An org that wrote 900
// before the deployment started forcing 600 keeps its 900 in `orgs.settings`, and
// the API reports both — the effective 600 with origin `config`, and the shadowed
// 900 beside it. Deleting the override on the operator's behalf would be a write
// nobody asked for; hiding it is how somebody spends an afternoon working out why
// the number they can see in the database is not the number in force.

// OriginConfig means a declarative source — an environment variable or a config
// file key — is forcing this value, and the API will refuse a write to it.
//
// It joins `default` and `org` on Origin. The enum is still closed and still
// small: three sources of a value, no hierarchy, no inheritance.
const OriginConfig Origin = "config"

// DeclaredEntry is one tuning key stated by configuration, with the config key
// that stated it.
//
// It is defined HERE, and `platform/config` defines its own twin, because domain
// packages own their vocabulary and do not import the configuration loader. The
// composition root maps between the two, which is four lines and keeps the
// dependency pointing the way CONTEXT.md §5.2 requires.
type DeclaredEntry struct {
	// Key is the tuning key as `orgs.settings` spells it.
	Key string
	// ConfigKey is where an operator would go to change it —
	// `OTO_TUNING_REFIRE_GRACE_S` or `tuning.refire_grace_s`. It is rendered into
	// the settings screen and into the 409 a write receives.
	ConfigKey string
	// Value is the raw configured value: a string from the environment, a typed
	// scalar or a list from YAML.
	Value any
}

// Declarative is the resolved declarative layer: a validated patch plus, per key,
// the config key that set it.
//
// The zero value is the ordinary case — an install that configures nothing — and
// every method is safe on it. That matters: this type sits on the read path of
// every tuning lookup in the process.
type Declarative struct {
	patch      SettingsPatch
	configKeys map[SettingKey]string
}

// NewDeclarative validates the declarative layer and resolves it into a patch.
//
// ⛔ IT REFUSES AT BOOT RATHER THAN DEGRADING AT RUNTIME. An unknown key, a
// value of the wrong type and a value outside its bound are all errors, and every
// one of them names the config key. The alternative — ignoring what it cannot
// understand — ships a deployment whose values file contains a line that does
// nothing and says nothing, which is the same class of failure as the silent
// revert this layer exists to remove.
//
// ⚠️ THE BOUNDS ARE THE SAME TABLE THE API ENFORCES. A declarative value is not
// privileged: `refire_grace_s: 0` is refused from a values file exactly as it is
// refused from `curl`, because the reason it is refused — a Slack thread per
// transition — does not care who asked.
func NewDeclarative(entries []DeclaredEntry) (Declarative, error) {
	out := Declarative{configKeys: map[SettingKey]string{}}
	if len(entries) == 0 {
		return out, nil
	}

	known := make(map[string]SettingKey, len(AllSettingKeys()))
	for _, k := range AllSettingKeys() {
		known[string(k)] = k
	}

	var v []errs.Violation
	for _, e := range entries {
		key, ok := known[e.Key]
		if !ok {
			v = append(v, errs.Violation{
				Field: e.ConfigKey, Code: "unknown_key",
				Message: e.Key + " is not a tuning key; the keys are " + strings.Join(settingKeyNames(), ", "),
			})
			continue
		}
		if err := out.set(key, e); err != nil {
			v = append(v, errs.Violation{
				Field: e.ConfigKey, Code: "invalid_value", Message: err.Error(),
			})
			continue
		}
		out.configKeys[key] = e.ConfigKey
	}
	if len(v) > 0 {
		return Declarative{}, errs.Validation("invalid_declarative_tuning",
			"the declarative tuning configuration is not usable", v...)
	}

	// The same table, applied to the same merged shape the API validates. A
	// declarative value that the API would have refused is refused here, at boot,
	// where an operator is watching a rollout rather than reading a support ticket.
	if err := out.patch.Validate(); err != nil {
		return Declarative{}, rekeyViolations(err, out.configKeys)
	}
	return out, nil
}

// set parses one entry into the patch, by the key's own type.
func (d *Declarative) set(key SettingKey, e DeclaredEntry) error {
	if ptr := (&d.patch).intPtr(key); ptr != nil {
		n, err := asInt(e.Value)
		if err != nil {
			return err
		}
		*ptr = &n
		return nil
	}

	switch key {
	case KeyDefaultVerbosity:
		s, err := asStr(e.Value)
		if err != nil {
			return err
		}
		d.patch.DefaultVerbosity = &s
	case KeyBroadcastOnResolved:
		b, err := asBool(e.Value)
		if err != nil {
			return err
		}
		d.patch.BroadcastOnResolved = &b
	default:
		return fmt.Errorf("%s cannot be set declaratively", key)
	}
	return nil
}

// Manages reports whether configuration is forcing this key.
func (d Declarative) Manages(k SettingKey) bool {
	_, ok := d.configKeys[k]
	return ok
}

// ConfigKey returns where a managed key was set, so a caller can tell an operator
// which line to edit. It returns "" for a key configuration does not manage.
func (d Declarative) ConfigKey(k SettingKey) string { return d.configKeys[k] }

// Keys returns the declaratively-managed keys in a stable order.
func (d Declarative) Keys() []SettingKey {
	out := make([]SettingKey, 0, len(d.configKeys))
	for _, k := range AllSettingKeys() {
		if d.Manages(k) {
			out = append(out, k)
		}
	}
	return out
}

// Patch is the declarative layer as a patch, for merging OVER an org's own.
//
// It returns a copy of the slice-valued field so that no caller can reach into
// the resolved layer and edit the deployment's stated configuration through it.
func (d Declarative) Patch() SettingsPatch {
	// ⛔ IT USED TO DEEP-COPY `UnackedReminderMentionList`, THE ONE SLICE-VALUED
	// FIELD, so no caller could edit the deployment's stated configuration through
	// the resolved layer. That field is gone (git-bug bd0fb1d) and no slice-valued
	// setting is left, so the copy has nothing to protect. ⚠️ IF ONE COMES BACK,
	// THE COPY COMES BACK WITH IT — a `SettingsPatch` returned by value still
	// shares any slice it holds.
	return d.patch
}

// Empty reports whether configuration manages nothing, which is the state of
// every install that has not opted in.
func (d Declarative) Empty() bool { return len(d.configKeys) == 0 }

// settingKeyNames is the closed key set as strings, for an error that has to
// tell somebody what they should have typed.
func settingKeyNames() []string {
	keys := AllSettingKeys()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// rekeyViolations restates a bounds failure in the operator's own vocabulary.
//
// `SettingsPatch.Validate` reports `refire_grace_s`, which is right for an API
// caller and wrong for somebody holding a values file: the field they can act on
// is `OTO_TUNING_REFIRE_GRACE_S`. The MESSAGE — the reason the bound is what it is
// — is carried through untouched, because that reason is identical either way.
func rekeyViolations(err error, configKeys map[SettingKey]string) error {
	vs := errs.ViolationsOf(err)
	out := make([]errs.Violation, 0, len(vs))
	for _, v := range vs {
		if ck, ok := configKeys[SettingKey(v.Field)]; ok {
			v.Field = ck
		}
		out = append(out, v)
	}
	return errs.Validation("invalid_declarative_tuning",
		"the declarative tuning configuration is outside the range oto will accept", out...)
}

// ------------------------------------------------------------------- coercion
//
// Environment variables are strings and YAML is typed, so the same key arrives as
// `"600"` from one provider and `600` from the other. These four accept both and
// refuse everything else BY NAME — a values file that says `refire_grace_s: "ten
// minutes"` must fail the rollout, not boot with a default and a shrug.

func asInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		if t != float64(int(t)) {
			return 0, fmt.Errorf("%v is not a whole number", t)
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("%q is not a whole number", t)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("%v is not a whole number", v)
	}
}

func asStr(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%v is not a string", v)
	}
	return strings.TrimSpace(s), nil
}

func asBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("%q is not true or false", t)
		}
		return b, nil
	default:
		return false, fmt.Errorf("%v is not true or false", v)
	}
}

// ⛔ `asList` WAS HERE AND IS DELETED (git-bug bd0fb1d). It accepted the three
// shapes a list-valued setting could arrive in — a YAML sequence, the []string a
// comma-separated environment value becomes, and a single bare string with no
// comma to split on. `unacked_reminder_mention_list` was the only list-valued
// setting, and it went with the reminder.
//
// ⚠️ IF A LIST-VALUED SETTING COMES BACK, SO DOES THIS: the bare-string case is
// the one a naive `[]any` cast misses, and it is the common case — a one-member
// list typed into an environment variable.

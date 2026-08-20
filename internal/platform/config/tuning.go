package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	kenv "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// The DECLARATIVE tuning layer: the third and highest source of a tuning knob.
//
// ⭐⭐ WHY THIS EXISTS. Until now a knob was either oto's shipped default or an
// org override in Postgres, and an install driven by Helm values, a config file
// or environment variables had no way to state a value at all. The failure that
// produced was silent and expensive: somebody edits the number in the UI, the
// next deploy rolls the pod, and the number is whatever the database still says —
// or, worse, whatever a future release decides to reconcile it to. Nobody can
// work out why the setting "changed back", because nothing anywhere says the
// deployment had an opinion.
//
// ⛔ DECLARATIVE WINS, AND IT WINS LOUDLY. Precedence is
//
//	shipped default  →  org override (Postgres)  →  declarative config
//
// with declarative highest, because that is the GitOps expectation: if it is in
// the values file, it is authoritative. A write to a declaratively-managed key is
// REFUSED with 409 rather than accepted and reverted — see
// `identity/service.UpdateOrgSettings`.
//
// ⭐ THE ORIGIN IS HALF THE FEATURE. `TuningEntry.ConfigKey` records the EXACT
// key that set the value — `OTO_TUNING_RESOLVE_GRACE_S` for an environment
// variable, `tuning.resolve_grace_s` for a file key. A badge saying "managed by
// configuration" and nothing else is a wall: the operator has to know WHERE TO GO
// AND CHANGE IT, and only the loader knows which of the two providers won.
//
// ⚠️ THIS PACKAGE DOES NOT KNOW WHAT A TUNING KEY MEANS. It harvests whatever
// sits under `tuning.`, verbatim, and hands it over. The closed key set, the
// types and the bounds live in `identity/domain`, which is the one place that
// already owns them — a second copy here is a second copy that can disagree.
// An unknown key is therefore not rejected here; it is rejected at boot by
// `identity/domain.NewDeclarative`, which can say what the legal keys are.

// TuningSection is the config section, and the env infix, that carries the
// declarative tuning layer: `tuning.resolve_grace_s` and
// `OTO_TUNING_RESOLVE_GRACE_S` are the same key.
const TuningSection = "tuning"

// TuningEntry is one declaratively-configured tuning value, with the config key
// that set it.
//
// The pairing is the type's whole reason to exist. A bare map of values would
// force every consumer to guess whether `600` arrived from the environment or
// from the file, and the guess is exactly what an operator needs answered.
type TuningEntry struct {
	// Key is the tuning key as `orgs.settings` spells it, e.g. `resolve_grace_s`.
	Key string
	// ConfigKey is where the value came from, spelled the way an operator would
	// have to type it: `OTO_TUNING_RESOLVE_GRACE_S` or `tuning.resolve_grace_s`.
	ConfigKey string
	// Value is koanf's resolved value — a string from the environment, a typed
	// scalar or a list from YAML. Parsing is the consumer's business.
	Value any
}

// TuningEntries returns the declaratively-configured tuning values in a stable
// key order. It is empty for an install that configures none, which is every
// install that has not opted in.
func (c Config) TuningEntries() []TuningEntry {
	out := make([]TuningEntry, len(c.tuning))
	copy(out, c.tuning)
	return out
}

// envTuningPrefix is the environment prefix one tuning key carries in full.
var envTuningPrefix = EnvPrefix + strings.ToUpper(TuningSection) + "_"

// loadTuning harvests the declarative layer and records WHICH provider set each
// key.
//
// It re-reads the file and the environment into two throwaway koanf instances
// rather than inspecting the merged one, because the merged instance cannot
// answer the only question that matters here. Merged, `tuning.resolve_grace_s`
// exists and holds 600; it does not say whether an operator would find that 600
// in their values file or in a Deployment's env block, and sending them to the
// wrong one is worse than saying nothing.
//
// The environment is read SECOND and wins, which mirrors Load's own binding
// order exactly. A key set in both places reports the environment variable,
// because the environment variable is the one in force.
func loadTuning(path string) ([]TuningEntry, error) {
	byKey := make(map[string]TuningEntry)

	if path != "" {
		fk := koanf.New(".")
		if err := fk.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("config: load file %q: %w", path, err)
		}
		for key, val := range flatTuning(fk) {
			byKey[key] = TuningEntry{
				Key:       key,
				ConfigKey: TuningSection + "." + key,
				Value:     val,
			}
		}
	}

	ek := koanf.New(".")
	if err := ek.Load(tuningEnvProvider(), nil); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}
	for key, val := range flatTuning(ek) {
		byKey[key] = TuningEntry{
			Key:       key,
			ConfigKey: envTuningPrefix + strings.ToUpper(key),
			Value:     val,
		}
	}

	out := make([]TuningEntry, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// tuningEnvProvider reads only OTO_TUNING_* and keeps everything after the
// infix as ONE flat key, so `OTO_TUNING_FLAP_DIGEST_INTERVAL_S` does not get
// chopped into a nested path by an underscore that means nothing.
//
// ⚠️ THE EXAMPLE USED TO BE `OTO_TUNING_UNACKED_REMINDER_MENTION_MIN_SEVERITY`,
// which is a key that no longer exists (git-bug bd0fb1d, migration 00068). The
// substitution is not cosmetic: this comment is the only place in the loader that
// spells a whole environment variable out, so it reads as the worked example, and a
// worked example built on a key `identity/domain.NewDeclarative` now refuses with
// `unknown_key` teaches an operator a line that fails their boot. The mechanical
// point is unchanged and the substitute makes it just as well — four
// underscore-separated words after the infix, none of them a nesting level.
func tuningEnvProvider() koanf.Provider {
	return kenv.Provider(".", kenv.Opt{
		Prefix: envTuningPrefix,
		TransformFunc: func(key, value string) (string, any) {
			key = TuningSection + "." + strings.ToLower(strings.TrimPrefix(key, envTuningPrefix))
			if strings.Contains(value, ",") {
				return key, splitList(value)
			}
			return key, value
		},
	})
}

// flatTuning returns the `tuning.*` keys of one koanf instance, stripped of the
// section prefix. A nested value under `tuning` is flattened to a dotted key so
// that it still reaches the consumer and is rejected there BY NAME, rather than
// vanishing here and leaving an operator with a values file whose key does
// nothing and says nothing.
func flatTuning(k *koanf.Koanf) map[string]any {
	out := map[string]any{}
	for path, val := range k.All() {
		if !strings.HasPrefix(path, TuningSection+".") {
			continue
		}
		out[strings.TrimPrefix(path, TuningSection+".")] = val
	}
	return out
}

// splitList splits a comma-separated environment value and trims each element.
//
// ⛔ NO SURVIVING TUNING KEY IS LIST-VALUED, and this is deliberately still here.
// It existed for `unacked_reminder_mention_list`, the only list-valued setting oto
// ever had, and that key went with the unacked reminder (git-bug bd0fb1d,
// migration 00068) — `identity/domain`'s `asList` was deleted outright at the same
// time. What survives here is not that key's plumbing but a TRANSPORT rule, and it
// is the half that has to live on this side of the seam: a comma is the only list
// syntax an environment variable has, and this package is key-agnostic by design
// (see the ⚠️ at the top), so it cannot ask whether the key it is holding wants
// one. Every remaining key receives the slice and rejects it BY NAME in
// `identity/domain.NewDeclarative` — `[900 1200] is not a whole number` — which is
// the correct answer to a comma in a number and is strictly better than silently
// parsing `900,1200` as nine hundred.
//
// ⚠️ IF A LIST-VALUED SETTING COMES BACK, THIS IS ALREADY WAITING FOR IT, and
// `asList` is the half that has to come back with it. The pairing is stated in
// `identity/domain/declarative.go`'s closing tombstone; this is its other end.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

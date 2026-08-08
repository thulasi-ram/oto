package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The declarative tuning layer's loader. What is asserted here is PROVENANCE, not
// values: the value is easy and the provenance is the feature. An operator told
// "managed by configuration" and nothing else has to search a Helm chart, a values
// file and a Deployment's env block to find the line they are allowed to edit.

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oto.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestNoTuningConfiguredYieldsNoEntries — the ordinary install.
func TestNoTuningConfiguredYieldsNoEntries(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.TuningEntries(); len(got) != 0 {
		t.Fatalf("entries %+v, want none", got)
	}
}

// TestAFileKeyReportsItsFilePath.
func TestAFileKeyReportsItsFilePath(t *testing.T) {
	path := writeConfig(t, "tuning:\n  refire_grace_s: 900\n  default_verbosity: firing_only\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entries := cfg.TuningEntries()
	if len(entries) != 2 {
		t.Fatalf("entries %+v, want two", entries)
	}
	// Stable key order, so a UI rendering them twice gets the same list twice.
	if entries[0].Key != "default_verbosity" || entries[1].Key != "refire_grace_s" {
		t.Fatalf("entries are not in key order: %+v", entries)
	}
	if entries[1].ConfigKey != "tuning.refire_grace_s" {
		t.Fatalf("config key %q, want the file path", entries[1].ConfigKey)
	}
	if entries[1].Value != 900 {
		t.Fatalf("value %v (%T), want 900 from YAML", entries[1].Value, entries[1].Value)
	}
}

// TestAnEnvVarReportsItsOwnName, spelled the way an operator would type it.
func TestAnEnvVarReportsItsOwnName(t *testing.T) {
	t.Setenv("OTO_TUNING_UNACKED_REMINDER_MENTION_MIN_SEVERITY", "warning")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entries := cfg.TuningEntries()
	if len(entries) != 1 {
		t.Fatalf("entries %+v, want one", entries)
	}
	if entries[0].Key != "unacked_reminder_mention_min_severity" {
		t.Fatalf("key %q: the trailing underscores are part of the key, not a nested path",
			entries[0].Key)
	}
	if entries[0].ConfigKey != "OTO_TUNING_UNACKED_REMINDER_MENTION_MIN_SEVERITY" {
		t.Fatalf("config key %q", entries[0].ConfigKey)
	}
}

// TestTheEnvironmentWinsAndSaysSo is the one that matters.
//
// A key set in both places must report the ENVIRONMENT VARIABLE, because the
// environment variable is the one in force. Reporting the file would send an
// operator to edit a line that is already being overridden, which is worse than
// saying nothing — they would change it, deploy, and watch nothing happen.
func TestTheEnvironmentWinsAndSaysSo(t *testing.T) {
	path := writeConfig(t, "tuning:\n  refire_grace_s: 900\n")
	t.Setenv("OTO_TUNING_REFIRE_GRACE_S", "1200")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entries := cfg.TuningEntries()
	if len(entries) != 1 {
		t.Fatalf("entries %+v, want one merged entry", entries)
	}
	if entries[0].ConfigKey != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config key %q, want the env var that is actually in force", entries[0].ConfigKey)
	}
	if entries[0].Value != "1200" {
		t.Fatalf("value %v, want the env var's", entries[0].Value)
	}
}

// TestACommaSeparatedEnvValueBecomesAList, which is the mention list's shape.
func TestACommaSeparatedEnvValueBecomesAList(t *testing.T) {
	t.Setenv("OTO_TUNING_UNACKED_REMINDER_MENTION_LIST", "<@U012AB3CD>, <!subteam^S01ABCDEF>")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entries := cfg.TuningEntries()
	if len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	list, ok := entries[0].Value.([]string)
	if !ok || len(list) != 2 {
		t.Fatalf("value %v (%T), want a two-element list", entries[0].Value, entries[0].Value)
	}
	if list[1] != "<!subteam^S01ABCDEF>" {
		t.Fatalf("the element was not trimmed: %q", list[1])
	}
}

// TestTheTuningSectionDoesNotLeakIntoTheRestOfTheConfig. `tuning` is harvested
// raw and has no field on Config; a stray key under it must not be able to
// disturb anything else the process reads.
func TestTheTuningSectionDoesNotLeakIntoTheRestOfTheConfig(t *testing.T) {
	path := writeConfig(t, "tuning:\n  refire_grace_s: 900\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTP.Addr != Default().HTTP.Addr {
		t.Fatalf("http.addr = %q, want the default", cfg.HTTP.Addr)
	}
	if cfg.Security.SessionCookie != Default().Security.SessionCookie {
		t.Fatalf("security.session_cookie = %q", cfg.Security.SessionCookie)
	}
}

// TestTuningEntriesHandsBackACopy. Config travels by value into every module;
// a caller that could edit the slice would edit the deployment's own record of
// what it configured.
func TestTuningEntriesHandsBackACopy(t *testing.T) {
	t.Setenv("OTO_TUNING_REFIRE_GRACE_S", "900")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	first := cfg.TuningEntries()
	first[0].ConfigKey = "somewhere_else"

	if got := cfg.TuningEntries()[0].ConfigKey; got != "OTO_TUNING_REFIRE_GRACE_S" {
		t.Fatalf("config key %q: the stored provenance was mutated by a caller", got)
	}
}

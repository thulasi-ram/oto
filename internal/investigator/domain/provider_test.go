package domain

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeBaseURL(t *testing.T) {
	good := map[string]string{
		"https://API.Example.test/v1/":     "https://api.example.test/v1",
		"  http://vllm.models.svc:8000/v1": "http://vllm.models.svc:8000/v1",
		"HTTPS://gateway.example.test":     "https://gateway.example.test",
	}
	for in, want := range good {
		got, err := NormalizeBaseURL(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "ftp://x.test", "https://", "not a url",
		"https://user:sk-secret@x.test/v1", // ⛔ a key in the URL is refused, not stripped
		"https://x.test/v1?api-version=1", "https://x.test/v1#frag",
		"https://x.test/" + strings.Repeat("a", MaxBaseURLLength),
	}
	for _, in := range bad {
		if _, err := NormalizeBaseURL(in); err == nil {
			t.Fatalf("NormalizeBaseURL(%q) accepted", in)
		}
	}
}

func TestProviderDraftNormalize(t *testing.T) {
	d, err := ProviderDraft{Name: " gateway ", BaseURL: "https://GW.test/v1/", Model: " m ", APIKey: " sk-1 "}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "gateway" || d.BaseURL != "https://gw.test/v1" || d.Model != "m" || d.APIKey != "sk-1" {
		t.Fatalf("draft = %#v", d.Name+"|"+d.BaseURL+"|"+d.Model)
	}
	for name, bad := range map[string]ProviderDraft{
		"no name":   {BaseURL: "https://x.test", Model: "m"},
		"long name": {Name: strings.Repeat("n", MaxProviderNameLength+1), BaseURL: "https://x.test", Model: "m"},
		"no model":  {Name: "n", BaseURL: "https://x.test"},
		"long key":  {Name: "n", BaseURL: "https://x.test", Model: "m", APIKey: strings.Repeat("k", MaxAPIKeyLength+1)},
		"bad url":   {Name: "n", BaseURL: "x.test", Model: "m"},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A keyless endpoint is legal, over http too.
	if _, err := (ProviderDraft{Name: "n", BaseURL: "http://ollama:11434/v1", Model: "m"}).Normalize(); err != nil {
		t.Fatalf("keyless draft refused: %v", err)
	}
	// ⛔ A key over plaintext is not.
	if _, err := (ProviderDraft{Name: "n", BaseURL: "http://litellm:4000", Model: "m", APIKey: "sk"}).Normalize(); err == nil {
		t.Fatal("a key bound for an http endpoint was accepted")
	}
}

func TestProviderDraftNeverPrintsItsKey(t *testing.T) {
	d := ProviderDraft{Name: "n", BaseURL: "https://x.test", Model: "m", APIKey: "sk-live-very-secret"}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("draft", "draft", d)
	for _, rendered := range []string{fmt.Sprint(d), fmt.Sprintf("%+v", d), fmt.Sprintf("%#v", d), buf.String()} {
		if strings.Contains(rendered, "sk-live") {
			t.Fatalf("the key leaked: %s", rendered)
		}
	}
}

func TestProviderConfigIdentity(t *testing.T) {
	c := ProviderConfig{ID: uuid.New(), BaseURL: "https://gw.test/v1", Model: "m"}
	if got := c.Identity(); got.Endpoint != "https://gw.test/v1" || got.Model != "m" || got.String() != "https://gw.test/v1#m" {
		t.Fatalf("identity = %+v", got)
	}
	// The identity is the endpoint and model, never the row id: the same URL and model
	// under a new id is the same model.
	if c.Identity() != (ProviderConfig{ID: uuid.New(), BaseURL: c.BaseURL, Model: c.Model}).Identity() {
		t.Fatal("identity depends on the row id")
	}
	if c.HasKey() {
		t.Fatal("a config with no credential claims a key")
	}
}

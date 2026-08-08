package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ⛔⛔ `response_url` ARRIVES INSIDE THE REQUEST BODY.
//
// The HMAC proves Slack sent the envelope. It does not prove the URL inside it
// is safe to POST to, and a URL taken out of a request body and dialled is the
// textbook shape of a server-side request forgery: point it at a link-local
// metadata service and oto fetches credentials on somebody else's behalf.
//
// These are the tests for the one control that stops it.

func TestEphemeralRefusesAnyHostThatIsNotSlack(t *testing.T) {
	// A transport that fails loudly if it is ever reached. Refusal must happen
	// BEFORE any resolution or dial, so nothing here should ever run.
	tripped := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		tripped = true
		return nil, http.ErrUseLastResponse
	})}
	n := NewNotice(client)

	refused := []struct {
		name string
		url  string
	}{
		{"the AWS metadata service", "http://169.254.169.254/latest/meta-data/iam/security-credentials/"},
		{"loopback", "http://127.0.0.1:8080/admin"},
		{"an attacker's host", "https://evil.example.com/collect"},
		{"a lookalike suffix", "https://hooks.slack.com.evil.example.com/x"},
		{"a lookalike prefix", "https://nothooks.slack.com/x"},
		{"plain http on the right host", "http://hooks.slack.com/actions/T1/2/3"},
		{"a file URL", "file:///etc/passwd"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if err := n.Ephemeral(context.Background(), tc.url, "hello"); err == nil {
				t.Fatalf("%s was accepted; an unchecked response_url is an SSRF hole", tc.url)
			}
			if tripped {
				t.Fatalf("%s was DIALLED before it was refused", tc.url)
			}
		})
	}
}

func TestEphemeralPostsAnEphemeralMessageThatDoesNotReplaceTheCard(t *testing.T) {
	var got struct {
		ResponseType    string `json:"response_type"`
		ReplaceOriginal bool   `json:"replace_original"`
		Text            string `json:"text"`
	}
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// The host allow-list is the point of the previous test, so this one rewrites
	// the request onto the local server rather than weakening the list.
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}

	n := NewNotice(client)
	err := n.Ephemeral(context.Background(),
		"https://hooks.slack.com/actions/T9TK3CUKW/1/2", "That alert has already resolved.")
	if err != nil {
		t.Fatalf("Ephemeral: %v", err)
	}

	if gotPath != "/actions/T9TK3CUKW/1/2" {
		t.Fatalf("posted to %q", gotPath)
	}
	if got.ResponseType != "ephemeral" {
		t.Fatalf("response_type = %q, want ephemeral: a note about a button must not be visible to the channel", got.ResponseType)
	}
	// ⛔ THE LOAD-BEARING ONE. Slack's DEFAULT for a block action's response URL
	// is to REPLACE the message the button is on — which would delete the alert
	// card and leave an error sentence in its place. The card is the record.
	if got.ReplaceOriginal {
		t.Fatal("replace_original is true: answering a button would delete the alert card it sits on")
	}
	if got.Text == "" {
		t.Fatal("no text was sent")
	}
}

func TestEphemeralIsANoOpWithoutAURLOrText(t *testing.T) {
	n := NewNotice(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("nothing should have been sent")
		return nil, nil
	})})
	if err := n.Ephemeral(context.Background(), "", "hi"); err != nil {
		t.Fatalf("empty url: %v", err)
	}
	if err := n.Ephemeral(context.Background(), "https://hooks.slack.com/actions/1/2/3", ""); err != nil {
		t.Fatalf("empty text: %v", err)
	}
}

func TestEphemeralReportsARejectedResponseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// What an expired response_url actually answers.
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}
	err := NewNotice(client).Ephemeral(context.Background(),
		"https://hooks.slack.com/actions/T1/2/3", "hello")
	if err == nil {
		t.Fatal("a rejected response_url was reported as success")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

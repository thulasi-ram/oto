package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"

	slackprov "github.com/thulasiram/oto/internal/channels/providers/slack"
)

// Slack is a fake Slack Web API workspace.
//
// It serves the THREE methods oto is allowed to call — `chat.postMessage`,
// `chat.update` and `auth.test` — and nothing else. That is not an omission: the
// provider's `API` interface is deliberately narrow because everything oto can
// do to a workspace is a scope somebody has to approve, and ⛔ OTO NEVER READS
// SLACK BACK (C9, ADR 0008). A fake that offered `conversations.history` would
// let a test drift past a scope oto does not request.
//
// The fake is HTTP rather than a Go stub of `slackprov.API` so that the real
// slack-go client, the real `MsgOptionAttachments` marshalling and the real
// `ts`-as-a-string discipline are all in the path. `ts` is the field a float
// round-trip silently corrupts, and that is the single most common Slack
// integration bug — a Go-level fake cannot catch it.
type Slack struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	seq      int
	team     string
	botUser  string
	posts    []SlackMessage
	updates  []SlackMessage
	errQueue []string
}

// SlackMessage is one call the fake received, decoded from the form body Slack
// actually receives.
type SlackMessage struct {
	// Method is "chat.postMessage" or "chat.update".
	Method string
	// Token is the bearer token the SDK sent.
	Token string
	// Channel is the conversation id the call targeted.
	Channel string
	// TS is the message ts this call ADDRESSED: the target of an update, or the
	// thread root of a reply. Empty for a plain root post.
	TS string
	// ThreadTS is the thread root a reply was threaded off.
	ThreadTS string
	// ReplyBroadcast is Slack's "also send to channel" flag.
	ReplyBroadcast bool
	// Text is the top-level text — the push notification, the search snippet and
	// THE ONLY THING A SCREEN READER READS.
	Text string
	// Attachments is the raw `attachments` JSON, byte-for-byte as oto sent it.
	// Assert on this to prove the payload that was validated is the payload that
	// was sent.
	Attachments json.RawMessage
	// AssignedTS is the ts the fake answered with.
	AssignedTS string
}

// NewSlack starts a fake workspace and stops it when the test ends.
func NewSlack(t *testing.T) *Slack {
	t.Helper()
	s := &Slack{t: t, team: "T00000TEST", botUser: "U00000BOT"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat.postMessage", s.handlePost)
	mux.HandleFunc("/api/chat.update", s.handleUpdate)
	mux.HandleFunc("/api/auth.test", s.handleAuthTest)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// APIURL is the endpoint to hand slack-go. slack-go appends the method name
// directly, so the trailing slash is load-bearing.
func (s *Slack) APIURL() string { return s.srv.URL + "/api/" }

// NewAPI is the hook to pass as `slack.Options.NewAPI`, which is the seam the
// provider already declares for exactly this purpose:
//
//	slackprov.NewProvider(slackprov.Options{NewAPI: fake.NewAPI()})
//
// What it builds is the REAL slack-go client, merely pointed at the fake.
func (s *Slack) NewAPI() func(token string, httpClient *http.Client) slackprov.API {
	return func(token string, httpClient *http.Client) slackprov.API {
		opts := []slackapi.Option{slackapi.OptionAPIURL(s.APIURL())}
		if httpClient != nil {
			opts = append(opts, slackapi.OptionHTTPClient(httpClient))
		}
		return slackapi.New(token, opts...)
	}
}

// Provider builds the real Slack provider wired to this fake.
func (s *Slack) Provider(opts slackprov.Options) *slackprov.Provider {
	opts.NewAPI = s.NewAPI()
	return slackprov.NewProvider(opts)
}

// TeamID is the workspace id `auth.test` reports.
func (s *Slack) TeamID() string { return s.team }

// Config is a schema-valid `channels.config` for this fake workspace.
//
// It exists so a test does not have to re-derive the two patterns schema.json
// enforces (`^T[A-Z0-9]{2,}$`, `^[CGD][A-Z0-9]{2,}$`). ⛔ A CONVERSATION ID, NEVER
// A #NAME: a name is ambiguous, mutable, and resolves differently per token.
func (s *Slack) Config(conversationID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"team_id":%q,"conversation_id":%q}`, s.team, conversationID))
}

// Credential is a sealed-shaped bot-token credential for this fake workspace.
func (s *Slack) Credential(token string) (kind string, values map[string]string) {
	return slackprov.CredBotToken, map[string]string{"bot_token": token}
}

// Posts returns every chat.postMessage the fake received, in order.
func (s *Slack) Posts() []SlackMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SlackMessage(nil), s.posts...)
}

// Updates returns every chat.update the fake received, in order.
//
// ADR 0008 makes `chat.update` PRIMARY and a thread reply the exception, so a
// test that asserts "one post and four updates" is asserting the product's
// central behaviour, not an implementation detail.
func (s *Slack) Updates() []SlackMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SlackMessage(nil), s.updates...)
}

// FailNext queues Slack error codes, one per subsequent call, answering
// `{"ok":false,"error":<code>}` — which is how Slack reports failure, with HTTP
// 200. Use the real codes (`ratelimited`, `channel_not_found`, `is_archived`,
// `not_in_channel`, `invalid_auth`): §H.9 classifies them, and a made-up code
// proves nothing about the classification.
func (s *Slack) FailNext(codes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errQueue = append(s.errQueue, codes...)
}

func (s *Slack) handlePost(w http.ResponseWriter, r *http.Request) {
	msg, ok := s.record(w, r, "chat.postMessage")
	if !ok {
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "channel": msg.Channel, "ts": msg.AssignedTS,
		"message": map[string]any{"text": msg.Text, "ts": msg.AssignedTS},
	})
}

func (s *Slack) handleUpdate(w http.ResponseWriter, r *http.Request) {
	msg, ok := s.record(w, r, "chat.update")
	if !ok {
		return
	}
	// An update keeps the ts it addressed. Minting a new one here would hide the
	// bug where oto stores the wrong durable handle.
	writeJSON(w, map[string]any{
		"ok": true, "channel": msg.Channel, "ts": msg.TS, "text": msg.Text,
	})
}

func (s *Slack) handleAuthTest(w http.ResponseWriter, r *http.Request) {
	if code, failed := s.nextError(); failed {
		_ = r.Body.Close()
		writeJSON(w, map[string]any{"ok": false, "error": code})
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "url": "https://oto.slack.test/", "team": "oto test workspace",
		"user": "oto", "team_id": s.team, "user_id": s.botUser, "bot_id": "B00000BOT",
	})
}

// record parses the form body Slack receives and files the call.
func (s *Slack) record(w http.ResponseWriter, r *http.Request, method string) (SlackMessage, bool) {
	if err := r.ParseForm(); err != nil {
		body, _ := io.ReadAll(r.Body)
		s.t.Errorf("harness: slack %s: unparseable body: %v (%s)", method, err, body)
		writeJSON(w, map[string]any{"ok": false, "error": "invalid_form"})
		return SlackMessage{}, false
	}

	if code, failed := s.nextError(); failed {
		writeJSON(w, map[string]any{"ok": false, "error": code})
		return SlackMessage{}, false
	}

	broadcast, _ := strconv.ParseBool(r.PostForm.Get("reply_broadcast"))
	msg := SlackMessage{
		Method:         method,
		Token:          bearer(r),
		Channel:        r.PostForm.Get("channel"),
		TS:             r.PostForm.Get("ts"),
		ThreadTS:       r.PostForm.Get("thread_ts"),
		ReplyBroadcast: broadcast,
		Text:           r.PostForm.Get("text"),
	}
	if raw := r.PostForm.Get("attachments"); raw != "" {
		msg.Attachments = json.RawMessage(raw)
	}
	if msg.TS == "" {
		msg.TS = msg.ThreadTS
	}

	s.mu.Lock()
	s.seq++
	// A Slack ts is "<unix>.<six digits>" and is a STRING, always. The six-digit
	// tail is what a float round trip destroys.
	msg.AssignedTS = fmt.Sprintf("17000000%02d.%06d", s.seq, s.seq)
	if method == "chat.update" {
		s.updates = append(s.updates, msg)
	} else {
		s.posts = append(s.posts, msg)
	}
	s.mu.Unlock()

	return msg, true
}

func (s *Slack) nextError() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errQueue) == 0 {
		return "", false
	}
	code := s.errQueue[0]
	s.errQueue = s.errQueue[1:]
	return code, true
}

func bearer(r *http.Request) string {
	if v := r.PostForm.Get("token"); v != "" {
		return v
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

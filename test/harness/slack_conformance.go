package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	slackprov "github.com/thulasiram/oto/internal/channels/providers/slack"
)

// SlackConformance is a fake Slack Web API that ENFORCES the documented request
// contract instead of merely recording it.
//
// ⭐⭐ WHY THERE ARE NOW TWO SLACK FAKES, AND WHAT THE SECOND ONE IS FOR.
//
// `Slack` (fake_slack.go) is a RECORDER: it accepts whatever oto sends and files
// it so a test can assert on it. That is the right tool for "did the notification
// module post once and update four times", and it is used by the tests that ask
// that question.
//
// It is the wrong tool for the question this file exists to answer. oto has never
// had a Slack credential; every rule it obeys about Block Kit lives in
// `internal/channels/render/slack/validate.go` and is checked by oto against
// oto. A recorder cannot fail that closed loop, because a recorder agrees with
// whatever oto sends. ⛔ A FAKE THAT ACCEPTS EVERYTHING PROVES NOTHING.
//
// This double is the other side of the wire. It implements Slack's PUBLISHED
// constraints independently of oto's validator — required arguments, the token
// shape, the Block Kit limits from the Block Kit reference, `thread_ts`
// semantics, `chat.update` authorship, and the real error vocabulary — and it
// refuses anything that breaks them with the code Slack documents. When oto's
// belief and Slack's published contract disagree, a test gets `invalid_blocks`
// instead of a green tick.
//
// # What it can and cannot prove
//
// It proves oto's CLIENT: that the bytes on the wire satisfy the documented
// contract, that the threading state machine addresses the right ts, that an
// update targets a message this token authored, and that oto classifies the
// documented failures correctly.
//
// ⛔ IT PROVES NOTHING ABOUT RENDERING. Whether Slack's own client draws the card
// legibly — text truncation, emoji resolution, mrkdwn interpretation, the colour
// bar, what a broadcast's in-channel reference actually shows — is not a property
// of the request contract and cannot be derived from it. That residual needs a
// workspace and is written up as a human checklist in docs/setup/slack.md.
type SlackConformance struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	seq      int
	team     string
	botUser  string
	botID    string
	channels map[string]*SlackFakeChannel
	messages map[string]*slackStoredMessage // keyed by channel+":"+ts
	calls    []SlackCall
	errQueue []string
	throttle *slackThrottle
	// revoked is the set of tokens that now answer token_revoked.
	revoked map[string]bool
	// metadataLimit is off by default: Slack publishes no figure for
	// `metadata_too_large` and this double will not invent one.
	metadataLimit int
	// metadataNeedsAppToken turns on the pessimistic reading of
	// `metadata_must_be_sent_from_app`.
	metadataNeedsAppToken bool
	// flattenThreadOffReply models the UNDOCUMENTED behaviour of threading off a
	// reply. Slack says only "avoid using a reply's ts value; use its parent
	// instead" and states neither an error nor a coercion, so the default is the
	// widely reported one — silent re-parenting — and a test can turn it off to
	// assert the other reading.
	flattenThreadOffReply bool
}

// SetMetadataLimit turns on `metadata_too_large` at a byte ceiling the CALLER
// chooses. ⛔ Slack documents no number; passing one is asserting a guess, and
// the test that does it owns the guess.
func (s *SlackConformance) SetMetadataLimit(bytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadataLimit = bytes
}

// SetMetadataRequiresAppToken makes every message carrying `metadata` fail with
// `metadata_must_be_sent_from_app`, which is the pessimistic reading of Slack's
// own error text. It exists so a test can prove oto FAILS LOUDLY AND ONCE under
// the reading that would mean no card has ever been delivered.
func (s *SlackConformance) SetMetadataRequiresAppToken(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadataNeedsAppToken = on
}

// SlackFakeChannel is one conversation in the fake workspace, with the three
// properties that decide whether a post is legal.
type SlackFakeChannel struct {
	// ID is the conversation id. ⛔ NEVER A #NAME.
	ID string
	// Member is whether the bot has been invited. oto does not request
	// `chat:write.public`, so this is the difference between a delivery and
	// `not_in_channel`.
	Member bool
	// Archived makes every write fail with `is_archived`.
	Archived bool
	// ReadOnly is the workspace preference behind
	// `restricted_action_read_only_channel`.
	ReadOnly bool
	// NonThreadable refuses thread replies with
	// `restricted_action_non_threadable_channel`.
	NonThreadable bool
	// ThreadOnly refuses top-level messages with
	// `restricted_action_thread_only_channel`.
	ThreadOnly bool
}

// slackStoredMessage is a message the fake has accepted and now owns.
type slackStoredMessage struct {
	Channel string
	// TS is the message's own timestamp — a STRING, always.
	TS string
	// ThreadTS is the ROOT of the thread this message belongs to. It equals TS for
	// a root message, so "is this a reply" is `ThreadTS != TS`.
	ThreadTS string
	// AuthoredBy is the token that posted it. `chat.update` against a message a
	// different token wrote is `cant_update_message`.
	AuthoredBy string
	Broadcast  bool
	Text       string
	Updates    int
}

// SlackCall is one request the fake handled, decoded and judged.
type SlackCall struct {
	Method string
	Token  string

	Channel        string
	TS             string // chat.update target
	ThreadTS       string // as SENT by oto
	ReplyBroadcast bool
	Text           string
	Attachments    json.RawMessage
	Blocks         json.RawMessage
	Metadata       json.RawMessage
	UnfurlLinks    string
	UnfurlMedia    string
	LinkNames      string

	// AssignedTS is the ts the fake answered with. For chat.update it is the ts
	// that was addressed, because an edit does not mint a new one.
	AssignedTS string
	// EffectiveThreadTS is where Slack ACTUALLY put the message. ⚠️ It differs
	// from ThreadTS when oto threaded off a reply: Slack does not nest, it
	// re-parents to the thread root, and a client that relies on nesting is
	// silently flattened.
	EffectiveThreadTS string
	// Flattened records that re-parenting happened.
	Flattened bool
	// Truncated records that the message was ACCEPTED and silently shortened.
	Truncated bool

	Status int
	OK     bool
	// Error is Slack's error code when OK is false.
	Error string
	// RetryAfter is the header value on a 429.
	RetryAfter int
}

// NewSlackConformance starts a conforming fake workspace with one channel the
// bot has been invited to, and stops it when the test ends.
func NewSlackConformance(t *testing.T, conversationID string) *SlackConformance {
	t.Helper()
	s := &SlackConformance{
		t: t, team: "T00000TEST", botUser: "U00000BOT", botID: "B00000BOT",
		channels:              map[string]*SlackFakeChannel{},
		messages:              map[string]*slackStoredMessage{},
		revoked:               map[string]bool{},
		metadataLimit:         slackMetadataUnchecked,
		flattenThreadOffReply: true,
	}
	s.AddChannel(SlackFakeChannel{ID: conversationID, Member: true})

	mux := http.NewServeMux()
	// ⛔ EXACTLY THE THREE METHODS oto's `API` interface declares, and no more.
	// A fake that served `conversations.history` would let a test drift past a
	// scope the manifest does not request and oto must never acquire.
	mux.HandleFunc("/api/chat.postMessage", s.handle("chat.postMessage"))
	mux.HandleFunc("/api/chat.update", s.handle("chat.update"))
	mux.HandleFunc("/api/auth.test", s.handleAuthTest)
	mux.HandleFunc("/api/", s.handleUnknownMethod)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// AddChannel registers a conversation.
func (s *SlackConformance) AddChannel(c SlackFakeChannel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyOf := c
	s.channels[c.ID] = &copyOf
}

// APIURL is the endpoint to hand slack-go. The trailing slash is load-bearing:
// slack-go appends the method name to it directly.
func (s *SlackConformance) APIURL() string { return s.srv.URL + "/api/" }

// NewAPI is the `slack.Options.NewAPI` hook. What it builds is the REAL slack-go
// client, merely pointed at this fake, so the real form encoding, the real
// attachment marshalling and the real `ts`-as-a-string discipline are all in the
// path.
func (s *SlackConformance) NewAPI() func(token string, httpClient *http.Client) slackprov.API {
	return func(token string, httpClient *http.Client) slackprov.API {
		opts := []slackapi.Option{slackapi.OptionAPIURL(s.APIURL())}
		if httpClient != nil {
			opts = append(opts, slackapi.OptionHTTPClient(httpClient))
		}
		return slackapi.New(token, opts...)
	}
}

// Provider builds the real Slack provider wired to this fake.
func (s *SlackConformance) Provider(opts slackprov.Options) *slackprov.Provider {
	opts.NewAPI = s.NewAPI()
	return slackprov.NewProvider(opts)
}

// TeamID is the workspace id `auth.test` reports.
func (s *SlackConformance) TeamID() string { return s.team }

// Config is a schema-valid `channels.config` for this fake workspace.
func (s *SlackConformance) Config(conversationID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"team_id":%q,"conversation_id":%q}`, s.team, conversationID))
}

// Credential is a sealed-shaped bot-token credential.
func (s *SlackConformance) Credential(token string) (kind string, values map[string]string) {
	return slackprov.CredBotToken, map[string]string{"bot_token": token}
}

// Calls returns every request the fake handled, in order, accepted or refused.
func (s *SlackConformance) Calls() []SlackCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SlackCall(nil), s.calls...)
}

// CallsTo returns the handled requests for one method.
func (s *SlackConformance) CallsTo(method string) []SlackCall {
	out := make([]SlackCall, 0, 4)
	for _, c := range s.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Thread returns the ts of every message the fake placed in one thread, oldest
// first, starting with the root. It is how a test asserts the SHAPE of the
// conversation oto built rather than the calls it made.
func (s *SlackConformance) Thread(channel, rootTS string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, 4)
	for _, c := range s.calls {
		m := s.messages[channel+":"+c.AssignedTS]
		if m == nil || m.ThreadTS != rootTS || c.Method != "chat.postMessage" {
			continue
		}
		out = append(out, m.TS)
	}
	return out
}

// Message returns the fake's record of one message.
func (s *SlackConformance) Message(channel, ts string) (channelID, threadTS, author string, updates int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, found := s.messages[channel+":"+ts]
	if !found {
		return "", "", "", 0, false
	}
	return m.Channel, m.ThreadTS, m.AuthoredBy, m.Updates, true
}

// FailNext queues Slack error codes, one per subsequent call, answered as Slack
// answers them: HTTP 200 with `{"ok":false,"error":<code>}`.
//
// ⛔ USE REAL CODES. §H.9 classifies them by what oto must DO, and a made-up code
// proves nothing about the classification it exercises.
func (s *SlackConformance) FailNext(codes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errQueue = append(s.errQueue, codes...)
}

// RevokeToken makes every subsequent call with this token answer token_revoked.
func (s *SlackConformance) RevokeToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[token] = true
}

// slackThrottle is a fixed budget of calls before the fake starts answering 429.
type slackThrottle struct {
	method     string
	allow      int
	used       int
	retryAfter int
}

// Throttle makes the fake answer HTTP 429 with a `Retry-After` header once
// `allow` calls to `method` have succeeded.
//
// ⛔ IT IS AN HTTP 429 WITH A HEADER, NOT AN `ok:false` BODY. Slack signals rate
// limiting at the transport layer and the retry deadline is in `Retry-After`,
// which is exactly the shape `classify` has to recognise to honour it; an
// `{"ok":false,"error":"ratelimited"}` body carries no deadline and would let a
// bug that ignores the header pass.
func (s *SlackConformance) Throttle(method string, allow, retryAfterSeconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle = &slackThrottle{method: method, allow: allow, retryAfter: retryAfterSeconds}
}

// ---------------------------------------------------------------- handlers

func (s *SlackConformance) handleUnknownMethod(w http.ResponseWriter, r *http.Request) {
	// ⛔ THE SURFACE IS CLOSED. Reaching here means oto called a method its own
	// `API` interface does not declare and the manifest requests no scope for.
	s.t.Errorf("harness: oto called an undeclared Slack method %q — the API interface "+
		"has three methods and deploy/slack/manifest.yaml requests one scope", r.URL.Path)
	writeJSON(w, map[string]any{"ok": false, "error": "unknown_method"})
}

func (s *SlackConformance) handleAuthTest(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token := bearer(r)
	if code := s.tokenError(token); code != "" {
		s.record(SlackCall{Method: "auth.test", Token: token, Status: 200, Error: code})
		writeJSON(w, map[string]any{"ok": false, "error": code})
		return
	}
	if code, failed := s.nextError(); failed {
		s.record(SlackCall{Method: "auth.test", Token: token, Status: 200, Error: code})
		writeJSON(w, map[string]any{"ok": false, "error": code})
		return
	}
	s.record(SlackCall{Method: "auth.test", Token: token, Status: 200, OK: true})
	writeJSON(w, map[string]any{
		"ok": true, "url": "https://oto.slack.test/", "team": "oto test workspace",
		"user": "oto", "team_id": s.team, "user_id": s.botUser, "bot_id": s.botID,
	})
}

func (s *SlackConformance) handle(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call := SlackCall{Method: method, Status: http.StatusOK}

		if r.Method != http.MethodPost {
			// Slack's Web API is POST-only for write methods.
			s.deny(w, &call, "invalid_arguments")
			return
		}
		if err := r.ParseForm(); err != nil {
			s.deny(w, &call, "invalid_form_data")
			return
		}

		call.Token = bearer(r)
		call.Channel = r.PostForm.Get("channel")
		call.TS = r.PostForm.Get("ts")
		call.ThreadTS = r.PostForm.Get("thread_ts")
		call.ReplyBroadcast, _ = strconv.ParseBool(r.PostForm.Get("reply_broadcast"))
		call.Text = r.PostForm.Get("text")
		call.UnfurlLinks = r.PostForm.Get("unfurl_links")
		call.UnfurlMedia = r.PostForm.Get("unfurl_media")
		call.LinkNames = r.PostForm.Get("link_names")
		if raw := r.PostForm.Get("attachments"); raw != "" {
			call.Attachments = json.RawMessage(raw)
		}
		if raw := r.PostForm.Get("blocks"); raw != "" {
			call.Blocks = json.RawMessage(raw)
		}
		if raw := r.PostForm.Get("metadata"); raw != "" {
			call.Metadata = json.RawMessage(raw)
		}

		// 1. Transport-level throttling comes first: Slack answers 429 before it
		//    looks at anything else.
		if retry, throttled := s.takeThrottle(method); throttled {
			call.Status = http.StatusTooManyRequests
			call.RetryAfter = retry
			s.record(call)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			return
		}

		// 2. Authentication.
		if code := s.tokenError(call.Token); code != "" {
			s.deny(w, &call, code)
			return
		}

		// 3. A queued failure stands in for whatever the test is exercising.
		if code, failed := s.nextError(); failed {
			s.deny(w, &call, code)
			return
		}

		// 4. The documented request contract.
		if code := s.checkRequest(method, &call); code != "" {
			s.deny(w, &call, code)
			return
		}

		s.accept(w, &call, method)
	}
}

// checkRequest is Slack's documented argument contract for the two write
// methods. Every branch names the error code Slack's own reference lists.
func (s *SlackConformance) checkRequest(method string, call *SlackCall) string {
	if call.Channel == "" {
		return "channel_not_found"
	}

	s.mu.Lock()
	ch := s.channels[call.Channel]
	s.mu.Unlock()

	switch {
	case ch == nil:
		return "channel_not_found"
	case ch.Archived:
		return "is_archived"
	case !ch.Member:
		return "not_in_channel"
	case ch.ReadOnly:
		return "restricted_action_read_only_channel"
	}

	// [DOC] A message needs SOMETHING to say. "The text field is not enforced as
	// required when using blocks or attachments" — so text alone, blocks alone or
	// attachments alone is legal, and none of the three is `no_text`.
	if strings.TrimSpace(call.Text) == "" && len(call.Blocks) == 0 && len(call.Attachments) == 0 {
		return "no_text"
	}

	// ⚠️ THE TWO METHODS DISAGREE ABOUT LONG TEXT AND THE DIFFERENCE IS NOT
	// COSMETIC. chat.update REFUSES above 4 000 with `msg_too_long`, which is a
	// dead delivery oto must classify. chat.postMessage carries no `msg_too_long`
	// at all and instead TRUNCATES above 40 000 — it succeeds, and the message
	// says less than oto asked it to, with no error to notice. A card that was
	// silently shortened is the §L.6 failure mode oto's whole render-validation
	// story exists to prevent, and it is the one Slack will do quietly.
	switch method {
	case "chat.update":
		if utf8.RuneCountInString(call.Text) > slackUpdateMaxTextChars {
			return "msg_too_long"
		}
	case "chat.postMessage":
		if utf8.RuneCountInString(call.Text) > slackPostTruncateChars {
			call.Truncated = true
			call.Text = string([]rune(call.Text)[:slackPostTruncateChars])
		}
	}

	if len(call.Metadata) > 0 {
		if code := s.checkSlackMetadata(call.Metadata); code != "" {
			return code
		}
	}

	if code := s.checkSlackAttachments(method, call.Attachments); code != "" {
		return code
	}
	if len(call.Blocks) > 0 {
		var blocks []json.RawMessage
		if err := json.Unmarshal(call.Blocks, &blocks); err != nil {
			return "invalid_blocks_format"
		}
		if code := checkSlackBlockSet(blocks, map[string]bool{}); code != "" {
			return code
		}
	}

	switch method {
	case "chat.postMessage":
		return s.checkPost(ch, call)
	case "chat.update":
		return s.checkUpdate(call)
	}
	return ""
}

func (s *SlackConformance) checkPost(ch *SlackFakeChannel, call *SlackCall) string {
	if call.ThreadTS == "" {
		if ch.ThreadOnly {
			return "restricted_action_thread_only_channel"
		}
		return ""
	}
	if ch.NonThreadable {
		return "restricted_action_non_threadable_channel"
	}

	s.mu.Lock()
	parent := s.messages[call.Channel+":"+call.ThreadTS]
	s.mu.Unlock()
	if parent == nil {
		// [INFERRED] Slack cannot thread off a message it does not have. The
		// postMessage error table does not name the code for this case; oto's §H.9
		// already treats `message_not_found` as a thread-pointer state transition
		// with a fresh-root recovery, which is the behaviour that must hold whatever
		// Slack calls it.
		return "message_not_found"
	}
	// ⚠️⚠️ THREADING OFF A REPLY: SLACK SAYS "DON'T" AND DOES NOT SAY WHAT HAPPENS.
	// The whole of the documentation is "avoid using a reply's ts value; use its
	// parent instead" — no error code, no stated coercion. The provider's
	// `threadRoot` comment asserts that Slack "silently flattens the thread", which
	// is the widely reported behaviour and is NOT a documented one.
	//
	// So this models the reported behaviour and LABELS the modelling: the call is
	// accepted, the message lands in the ROOT thread, and `Flattened` records that
	// oto's intent and Slack's action differed. A test asserts that oto never
	// causes it — which is the property oto controls — rather than asserting what
	// Slack does, which nothing offline can establish.
	call.EffectiveThreadTS = parent.ThreadTS
	call.Flattened = parent.ThreadTS != call.ThreadTS
	if call.Flattened && !s.flattenThreadOffReply {
		return "message_not_found"
	}
	return ""
}

func (s *SlackConformance) checkUpdate(call *SlackCall) string {
	if call.TS == "" {
		return "invalid_arguments"
	}
	if !slackTSRe.MatchString(call.TS) {
		// ⭐ A ts that lost its six-digit tail is the signature of a float round
		// trip, which is the single most common Slack integration bug. Slack
		// answers `message_not_found` because the mangled value addresses nothing.
		return "message_not_found"
	}

	s.mu.Lock()
	target := s.messages[call.Channel+":"+call.TS]
	s.mu.Unlock()
	if target == nil {
		return "message_not_found"
	}
	if target.AuthoredBy != call.Token {
		// [DOC] "Only messages posted by the authenticated user are able to be
		// updated using this method… Bot users may also update the messages they
		// post." → `cant_update_message`, "authenticated user does not have
		// permission to update this message".
		//
		// ⭐ THIS IS THE ONE THAT MATTERS FOR ADR 0008. Update-in-place is the
		// primary verb for the whole product, and it works only for as long as the
		// SAME bot token owns the root. A rotated credential, a second oto install
		// pointed at the same channel, or a card posted by a predecessor app all
		// produce this code — and oto's recovery for it (clear the pointer, post a
		// fresh root with a `continued` marker) is the thing this fake can prove.
		return "cant_update_message"
	}
	if call.ReplyBroadcast {
		// [DOC] chat.update accepts `reply_broadcast` — "broadcast an existing
		// thread reply to make it visible to everyone" — with one hard restriction:
		// `no_dual_broadcast_content_update`, "can't broadcast an old reply and
		// update the content at the same time".
		//
		// ADR 0020 names post-quietly-then-broadcast-later as the SANCTIONED
		// mechanism for a transition whose importance is not knowable at post time.
		// This is the wall that path runs into if it carries content, and it is
		// modelled here so the path is built against it rather than into it.
		if strings.TrimSpace(call.Text) != "" || len(call.Blocks) > 0 || len(call.Attachments) > 0 {
			return "no_dual_broadcast_content_update"
		}
		// [INFERRED] There is no reply to broadcast when the target is a thread
		// root. Slack's `cant_broadcast_message` ("unable to broadcast this
		// message") is the only code that fits; the mapping is not documented.
		if target.ThreadTS == target.TS {
			return "cant_broadcast_message"
		}
	}
	return ""
}

func (s *SlackConformance) accept(w http.ResponseWriter, call *SlackCall, method string) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	// A Slack ts is "<unix>.<six digits>" and is a STRING, always.
	assigned := fmt.Sprintf("17000000%02d.%06d", seq, seq)

	switch method {
	case "chat.postMessage":
		thread := assigned
		if call.ThreadTS != "" {
			thread = call.EffectiveThreadTS
		}
		s.messages[call.Channel+":"+assigned] = &slackStoredMessage{
			Channel: call.Channel, TS: assigned, ThreadTS: thread,
			AuthoredBy: call.Token, Broadcast: call.ReplyBroadcast, Text: call.Text,
		}
		call.AssignedTS = assigned
		call.EffectiveThreadTS = thread
	case "chat.update":
		m := s.messages[call.Channel+":"+call.TS]
		m.Updates++
		m.Text = call.Text
		if call.ReplyBroadcast {
			m.Broadcast = true
		}
		// ⛔ AN EDIT KEEPS THE ts IT ADDRESSED. Minting a new one here would hide
		// the bug where oto stores the wrong durable handle.
		call.AssignedTS = call.TS
		call.EffectiveThreadTS = m.ThreadTS
	}
	call.OK = true
	s.calls = append(s.calls, *call)
	s.mu.Unlock()

	body := map[string]any{
		"ok": true, "channel": call.Channel, "ts": call.AssignedTS,
		"message": map[string]any{"text": call.Text, "ts": call.AssignedTS},
	}
	if call.EffectiveThreadTS != "" && call.EffectiveThreadTS != call.AssignedTS {
		body["message"].(map[string]any)["thread_ts"] = call.EffectiveThreadTS
	}
	writeJSON(w, body)
}

func (s *SlackConformance) deny(w http.ResponseWriter, call *SlackCall, code string) {
	call.OK = false
	call.Error = code
	s.record(*call)
	// Slack reports application errors with HTTP 200 and `ok:false`.
	writeJSON(w, map[string]any{"ok": false, "error": code})
}

func (s *SlackConformance) record(call SlackCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *SlackConformance) nextError() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errQueue) == 0 {
		return "", false
	}
	code := s.errQueue[0]
	s.errQueue = s.errQueue[1:]
	return code, true
}

func (s *SlackConformance) takeThrottle(method string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.throttle == nil || s.throttle.method != method {
		return 0, false
	}
	if s.throttle.used < s.throttle.allow {
		s.throttle.used++
		return 0, false
	}
	return s.throttle.retryAfter, true
}

// tokenError is the auth contract: a bearer token, of the bot shape, not revoked.
func (s *SlackConformance) tokenError(token string) string {
	if token == "" {
		return "not_authed"
	}
	s.mu.Lock()
	revoked := s.revoked[token]
	s.mu.Unlock()
	if revoked {
		return "token_revoked"
	}
	switch {
	case strings.HasPrefix(token, "xoxb-"):
		return ""
	case strings.HasPrefix(token, "xapp-"), strings.HasPrefix(token, "xoxp-"):
		// ⛔ A SIGNING SECRET OR AN APP TOKEN IN THE AUTHORIZATION HEADER IS A
		// CREDENTIAL-CONFUSION BUG, and the provider's `botToken` has already had
		// one. Slack names it precisely, so the fake does too.
		return "not_allowed_token_type"
	default:
		return "invalid_auth"
	}
}

// ---------------------------------------------------- Slack's own Block Kit rules

// Slack's published limits, transcribed from the Block Kit reference, the
// composition-object reference and the two `chat.*` method pages.
//
// ⛔⛔ THIS IS DELIBERATELY A SECOND, INDEPENDENT TRANSCRIPTION. It must NOT
// import the constants in `internal/channels/render/slack`, and it must not be
// refactored to share them. The entire value of this file is that it disagrees
// with oto when oto is wrong; a shared constant makes the two agree by
// construction and turns the check back into the closed loop it exists to break.
//
// ⚠️ EVERY NUMBER HERE IS MARKED [DOC] OR [INFERRED]. A conformance double that
// enforces invented limits is not a conformance double, it is a second opinion
// with the same standing as the first — and this file exists precisely because
// oto's numbers turned out to include three of those.
const (
	slackMaxBlocks         = 50   // [DOC] "up to 50 blocks in each message"
	slackMaxBlockID        = 255  // [DOC] block_id
	slackMaxActionID       = 255  // [DOC] action_id
	slackMaxSectionText    = 3000 // [DOC] section.text.text
	slackMaxSectionFields  = 10   // [DOC] section.fields, "maximum number of items is 10"
	slackMaxFieldText      = 2000 // [DOC] section.fields[].text
	slackMaxContextItems   = 10   // [DOC] context.elements
	slackMaxActionItems    = 25   // [DOC] "a maximum of 25 elements in each action block"
	slackMaxButtonText     = 75   // [DOC] button.text.text
	slackMaxButtonValue    = 2000 // [DOC] button.value
	slackMaxURL            = 3000 // [DOC] button.url and option.url
	slackMaxOptionText     = 75   // [DOC] option.text.text
	slackMaxOptionValue    = 150  // [DOC] option.value — ⚠️ 150, NOT the button's 2000
	slackMaxOverflowOpts   = 5    // [DOC] "an array of up to five option objects"
	slackMaxTextObject     = 3000 // [DOC] text object, "minimum length is 1 and maximum 3000"
	slackMaxAltText        = 2000 // [DOC] image alt_text, and it is REQUIRED
	slackMaxImageTitleText = 2000 // [DOC] image title
	slackMaxHeaderText     = 150  // [DOC] header block
	slackMaxAttachments    = 100  // [DOC] too_many_attachments, "a maximum of 100"

	// [DOC] chat.update: "the text field cannot exceed 4,000 characters"
	// (`msg_too_long`). ⚠️ This is an UPDATE-only error. chat.postMessage lists no
	// `msg_too_long` at all.
	slackUpdateMaxTextChars = 4000
	// [DOC] The 2018 truncation changelog: Slack truncates "messages containing
	// more than 40,000 characters". ⚠️ TRUNCATES — it does not refuse. A too-long
	// postMessage succeeds and quietly says less than it was asked to.
	slackPostTruncateChars = 40000

	// [INFERRED] Slack publishes NO size for `metadata_too_large`, on the method
	// pages or in the metadata guide. Enforcing an invented number here would make
	// this file guilty of exactly what it exists to catch, so metadata size is NOT
	// checked by default; a test that wants to exercise the code path sets
	// MetadataLimitBytes explicitly and owns the assumption.
	slackMetadataUnchecked = 0
)

// slackOverflowMinOpts is 1 and is [INFERRED]. The current overflow reference
// states a maximum and no minimum — an older revision required two — so this
// rejects only the empty array, which is not a menu by any reading.
const slackOverflowMinOpts = 1

// slackTSRe is the shape of a Slack message timestamp: unix seconds, a dot, and
// SIX digits. The six digits are what a float round trip destroys.
var slackTSRe = regexp.MustCompile(`^\d{10,}\.\d{6}$`)

// slackHexColour is the attachment colour format Slack documents alongside the
// three keywords.
var slackHexColour = regexp.MustCompile(`^#?[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$`)

// slackMessageBlocks are the block types Slack accepts in a MESSAGE. ⚠️ `input`
// is surfaced in modals and App Home only; Slack rejects it in a message, which
// is a real restriction oto's whitelist happens to cover by being narrower.
var slackMessageBlocks = map[string]bool{
	"actions": true, "context": true, "divider": true, "file": true,
	"header": true, "image": true, "markdown": true, "rich_text": true,
	"section": true, "video": true,
}

// checkSlackMetadata enforces the SHAPE Slack documents. The SIZE is not
// checked unless a test opts in, because Slack publishes no figure.
func (s *SlackConformance) checkSlackMetadata(raw json.RawMessage) string {
	var md struct {
		EventType    string          `json:"event_type"`
		EventPayload json.RawMessage `json:"event_payload"`
	}
	if err := json.Unmarshal(raw, &md); err != nil {
		return "invalid_metadata_format"
	}
	// [DOC] Message metadata requires an `event_type`.
	if md.EventType == "" {
		return "invalid_metadata_format"
	}
	s.mu.Lock()
	limit := s.metadataLimit
	s.mu.Unlock()
	if limit > slackMetadataUnchecked && len(md.EventPayload) > limit {
		return "metadata_too_large"
	}
	// ⛔ WHAT THIS CANNOT MODEL, AND IT IS THE BIGGEST UNKNOWN ON THE SLACK PATH.
	// Both method pages carry `metadata_must_be_sent_from_app` — "message metadata
	// can only be posted or updated using an app-level token" — and oto sends
	// metadata on every card under a bot `xoxb-` token. If Slack means that
	// literally, every oto delivery has always failed. A fake cannot decide it:
	// the answer lives in Slack's authorisation layer and is not derivable from
	// the request. `SetMetadataRequiresAppToken` lets a test assert oto's BEHAVIOUR
	// under the pessimistic reading; only a workspace can say which reading is real.
	s.mu.Lock()
	strictToken := s.metadataNeedsAppToken
	s.mu.Unlock()
	if strictToken {
		return "metadata_must_be_sent_from_app"
	}
	return ""
}

type slackFakeAttachment struct {
	Color    string            `json:"color"`
	Fallback string            `json:"fallback"`
	Blocks   []json.RawMessage `json:"blocks"`
}

// checkSlackAttachments judges the `attachments` array.
//
// ⚠️ The error code is METHOD-SPECIFIC and the two tables really do differ:
// `invalid_attachments` appears on chat.update and NOT on chat.postMessage. A
// fake that answered the same code for both would let oto's classification look
// right while being tested against a Slack that does not exist.
func (s *SlackConformance) checkSlackAttachments(method string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	malformed := "invalid_blocks_format"
	if method == "chat.update" {
		malformed = "invalid_attachments"
	}

	var atts []slackFakeAttachment
	if err := json.Unmarshal(raw, &atts); err != nil {
		return malformed
	}
	if len(atts) > slackMaxAttachments {
		return "too_many_attachments"
	}
	// [DOC] block_id uniqueness is "for each message and each iteration of a
	// message", so the seen-set spans every attachment rather than one block list.
	seen := map[string]bool{}
	for _, a := range atts {
		// [INFERRED] Slack documents the accepted colour forms — `good`, `warning`,
		// `danger` or "any hex color code (eg. #439FE0)" — but does NOT document
		// what it does with an unaccepted one. It very likely ignores it. This
		// rejects, which is the STRICTER reading; a test relying on it is relying
		// on inference.
		switch a.Color {
		case "", "good", "warning", "danger":
		default:
			if !slackHexColour.MatchString(a.Color) {
				return malformed
			}
		}
		if code := checkSlackBlockSet(a.Blocks, seen); code != "" {
			return code
		}
	}
	return ""
}

type slackFakeBlock struct {
	Type     string            `json:"type"`
	BlockID  string            `json:"block_id"`
	Text     *slackFakeText    `json:"text"`
	Fields   []slackFakeText   `json:"fields"`
	Elements []json.RawMessage `json:"elements"`
	ImageURL string            `json:"image_url"`
	AltText  string            `json:"alt_text"`
	SlackF   json.RawMessage   `json:"slack_file"`
	Title    *slackFakeText    `json:"title"`
}

type slackFakeText struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Emoji    *bool  `json:"emoji"`
	Verbatim *bool  `json:"verbatim"`
}

type slackFakeElement struct {
	Type     string            `json:"type"`
	ActionID string            `json:"action_id"`
	Text     *slackFakeText    `json:"text"`
	URL      string            `json:"url"`
	Value    string            `json:"value"`
	Style    string            `json:"style"`
	AltText  string            `json:"alt_text"`
	ImageURL string            `json:"image_url"`
	Options  []slackFakeOption `json:"options"`
}

type slackFakeOption struct {
	Text        *slackFakeText `json:"text"`
	Value       string         `json:"value"`
	URL         string         `json:"url"`
	Description *slackFakeText `json:"description"`
}

func checkSlackBlockSet(blocks []json.RawMessage, seen map[string]bool) string {
	if len(blocks) > slackMaxBlocks {
		return "invalid_blocks"
	}
	for _, raw := range blocks {
		var b slackFakeBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return "invalid_blocks_format"
		}
		if !slackMessageBlocks[b.Type] {
			return "invalid_blocks"
		}
		if b.BlockID != "" {
			if len(b.BlockID) > slackMaxBlockID || seen[b.BlockID] {
				return "invalid_blocks"
			}
			seen[b.BlockID] = true
		}
		if code := checkSlackBlock(b); code != "" {
			return code
		}
	}
	return ""
}

func checkSlackBlock(b slackFakeBlock) string {
	switch b.Type {
	case "section":
		if b.Text == nil && len(b.Fields) == 0 {
			return "invalid_blocks"
		}
		if b.Text != nil {
			if code := checkSlackText(*b.Text, slackMaxSectionText); code != "" {
				return code
			}
		}
		if len(b.Fields) > slackMaxSectionFields {
			return "invalid_blocks"
		}
		for _, f := range b.Fields {
			if code := checkSlackText(f, slackMaxFieldText); code != "" {
				return code
			}
		}

	case "context":
		if len(b.Elements) == 0 || len(b.Elements) > slackMaxContextItems {
			return "invalid_blocks"
		}
		for _, raw := range b.Elements {
			// ⛔ DISCRIMINATE ON `type` BEFORE DECODING THE BODY. A context element is
			// a TEXT OBJECT or an image element, and the two disagree about what
			// `text` is: on a text object `text` is a STRING, on the interactive
			// elements decoded elsewhere in this file it is an OBJECT. Decoding every
			// context element into the button-shaped `slackFakeElement` to read its
			// `type` fails on `{"type":"mrkdwn","text":"…"}` with "cannot unmarshal
			// string into …slackFakeText" and answers `invalid_blocks_format` for a
			// block Slack accepts — which is this double lying about Slack, the one
			// thing it must never do.
			var kind struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &kind); err != nil {
				return "invalid_blocks_format"
			}
			switch kind.Type {
			case "mrkdwn", "plain_text":
				var t slackFakeText
				if err := json.Unmarshal(raw, &t); err != nil {
					return "invalid_blocks_format"
				}
				// [DOC] The context block states no text length of its own, so the
				// generic text-object bound is the only documented one.
				if code := checkSlackText(t, slackMaxTextObject); code != "" {
					return code
				}
			case "image":
				// [DOC] An image element requires both alt_text and a source.
				var e slackFakeElement
				if err := json.Unmarshal(raw, &e); err != nil {
					return "invalid_blocks_format"
				}
				if e.AltText == "" || e.ImageURL == "" {
					return "invalid_blocks"
				}
			default:
				// [DOC] "An array of image elements and text objects." A context block
				// holds those two things; anything else — a button, notably — is not a
				// context element.
				return "invalid_blocks"
			}
		}

	case "actions":
		if len(b.Elements) == 0 || len(b.Elements) > slackMaxActionItems {
			return "invalid_blocks"
		}
		for _, raw := range b.Elements {
			var e slackFakeElement
			if err := json.Unmarshal(raw, &e); err != nil {
				return "invalid_blocks_format"
			}
			if code := checkSlackElement(e); code != "" {
				return code
			}
		}

	case "image":
		// alt_text is REQUIRED on an image block, and one of image_url or
		// slack_file must be present.
		if b.AltText == "" || len(b.AltText) > slackMaxAltText {
			return "invalid_blocks"
		}
		if b.ImageURL == "" && len(b.SlackF) == 0 {
			return "invalid_blocks"
		}
		if len(b.ImageURL) > slackMaxURL {
			return "invalid_blocks"
		}
		if b.Title != nil {
			// An image block title must be plain_text.
			if b.Title.Type != "plain_text" || len(b.Title.Text) > slackMaxImageTitleText {
				return "invalid_blocks"
			}
		}

	case "header":
		if b.Text == nil || b.Text.Type != "plain_text" {
			return "invalid_blocks"
		}
		if len(b.Text.Text) == 0 || len([]rune(b.Text.Text)) > slackMaxHeaderText {
			return "invalid_blocks"
		}

	case "divider", "rich_text", "file", "video", "markdown":
		// Nothing oto emits, and nothing worth a second transcription until it does.
	}
	return ""
}

func checkSlackText(t slackFakeText, maxLen int) string {
	switch t.Type {
	case "mrkdwn":
		// [INFERRED] `emoji` is documented as "only usable when type is
		// plain_text", and `verbatim` as mrkdwn-only — but Slack does NOT document
		// whether the wrong one is REJECTED or merely ignored. This rejects, which
		// is the stricter reading. It is worth being strict here: a client that sets
		// `emoji` on an mrkdwn object is depending on Slack ignoring a field its own
		// reference calls invalid, which is the weakest ground there is.
		if t.Emoji != nil {
			return "invalid_blocks"
		}
	case "plain_text":
		if t.Verbatim != nil {
			return "invalid_blocks"
		}
	default:
		// [DOC] A text object is plain_text or mrkdwn. There is no third type.
		return "invalid_blocks"
	}
	// [DOC] "The minimum length is 1 and maximum length is 3000 characters."
	if strings.TrimSpace(t.Text) == "" {
		return "invalid_blocks"
	}
	if utf8.RuneCountInString(t.Text) > maxLen {
		return "invalid_blocks"
	}
	return ""
}

func checkSlackElement(e slackFakeElement) string {
	if len(e.ActionID) > slackMaxActionID {
		return "invalid_blocks"
	}
	switch e.Type {
	case "button":
		if e.Text == nil {
			return "invalid_blocks"
		}
		// A button label is plain_text, always.
		if e.Text.Type != "plain_text" {
			return "invalid_blocks"
		}
		if code := checkSlackText(*e.Text, slackMaxButtonText); code != "" {
			return code
		}
		if len([]rune(e.Text.Text)) > slackMaxButtonText {
			return "invalid_blocks"
		}
		if len(e.URL) > slackMaxURL {
			return "invalid_blocks"
		}
		if len(e.Value) > slackMaxButtonValue {
			return "invalid_blocks"
		}
		switch e.Style {
		case "", "primary", "danger":
		default:
			return "invalid_blocks"
		}

	case "overflow":
		if len(e.Options) < slackOverflowMinOpts || len(e.Options) > slackMaxOverflowOpts {
			return "invalid_blocks"
		}
		for _, o := range e.Options {
			if o.Text == nil || o.Text.Type != "plain_text" {
				return "invalid_blocks"
			}
			if code := checkSlackText(*o.Text, slackMaxOptionText); code != "" {
				return code
			}
			if len([]rune(o.Text.Text)) > slackMaxOptionText {
				return "invalid_blocks"
			}
			// ⚠️ 150, NOT 2000. [DOC] The option object's `value` is "maximum length
			// for this field is 150 characters" — an order of magnitude shorter than a
			// button's 2000, and conflating the two is the single easiest Block Kit
			// limit to get wrong. (75 is the option's TEXT, checked just above.)
			if len(o.Value) > slackMaxOptionValue {
				return "invalid_blocks"
			}
			if len(o.URL) > slackMaxURL {
				return "invalid_blocks"
			}
		}

	default:
		// Every other interactive element is legal Block Kit; oto emits none of
		// them, and inventing a second transcription of rules nothing exercises
		// would be fiction rather than evidence.
	}
	return ""
}

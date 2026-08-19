package harness_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	chdomain "github.com/thulasiram/oto/internal/channels/domain"
	slackprov "github.com/thulasiram/oto/internal/channels/providers/slack"
	"github.com/thulasiram/oto/test/harness"
)

// ⭐⭐ THE FILE THAT ANSWERS git-bug edb670f AS FAR AS IT CAN BE ANSWERED OFFLINE.
//
// The issue's claim is exact and, as far as this repository can show, true: no
// Slack credential has ever been used, so the Block Kit card has never been
// rendered by Slack and ADRs 0008 and 0020 are untested claims about Slack's
// behaviour. The issue's "done when" — a real workspace, screenshots from a
// Slack client — is not achievable here and nothing below pretends otherwise.
//
// What IS achievable is splitting the untested claim in two.
//
//	CLIENT BEHAVIOUR — the bytes oto puts on the wire, the arguments it sets, the
//	  ts it addresses, the order it does things in, and what it does with each
//	  documented failure. All of that is a property of oto, checkable against
//	  Slack's PUBLISHED request contract, and it is what this file proves.
//
//	SLACK'S RENDERING — whether the card is legible, whether emoji resolve,
//	  whether mrkdwn is interpreted as intended, whether the colour bar appears,
//	  what a broadcast's in-channel reference actually shows. None of that is a
//	  property of the request, and no fake can establish it. It is the residual,
//	  and docs/setup/slack.md is the checklist for the person who can close it.
//
// ⛔ THE FAKE IS DELIBERATELY NOT A RECORDER. `harness.Slack` accepts whatever it
// is given; `harness.SlackConformance` refuses anything that breaks the
// documented contract, with the code Slack documents, using its OWN transcription
// of the limits. If oto's beliefs and Slack's reference disagree, these tests go
// red rather than green.

const conformanceChannel = "C0123456789"
const conformanceToken = "xoxb-conformance-test-token"

func newSlackConformanceChannel(t *testing.T) (*harness.SlackConformance, chdomain.Channel) {
	t.Helper()
	fake := harness.NewSlackConformance(t, conformanceChannel)
	provider := fake.Provider(slackprov.Options{})
	kind, values := fake.Credential(conformanceToken)
	channel, err := provider.Open(t.Context(),
		chdomain.ChannelConfig{Raw: fake.Config(conformanceChannel)},
		chdomain.Credential{Kind: kind, Values: values})
	if err != nil {
		t.Fatalf("open a slack channel against the conforming fake: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	return fake, channel
}

// card renders one of the checked-in variants through the REAL renderer, so what
// is driven down the wire is the payload oto would actually send rather than a
// hand-written stub that agrees with the test.
func card(t *testing.T, name string) chdomain.RenderedMessage {
	t.Helper()
	captures, err := harness.RenderSlackCards()
	if err != nil {
		t.Fatalf("render the card corpus: %v", err)
	}
	for _, c := range captures {
		if c.Card.Name == name {
			return chdomain.RenderedMessage{
				Fallback: c.Fallback, Payload: c.Wire, Hash: c.Hash,
			}
		}
	}
	t.Fatalf("no card variant named %q", name)
	return chdomain.RenderedMessage{}
}

func channelError(t *testing.T, err error) *chdomain.Error {
	t.Helper()
	var ce *chdomain.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a classified channels error; retry policy reads the "+
			"CLASS and nothing downstream re-inspects the SDK error", err)
	}
	return ce
}

// ---------------------------------------------------------------- ADR 0008

// ⭐ THE WHOLE OF ADR 0008 IN ONE SEQUENCE.
//
// One root post; a threaded reply; an update of the ROOT — not of the reply —
// and a second update carrying the terminal card. The assertions are about the
// two things ADR 0008 actually claims: that `chat.update` is the primary verb,
// and that `(channel_id, ts)` in oto's database is a durable handle that survives
// every edit unchanged.
func TestTheRootIsPostedOnceAndAmendedInPlaceForTheRestOfItsLife(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	ctx := t.Context()

	// ---- 1. THE ROOT ---------------------------------------------------
	root, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post the root card: %v", err)
	}

	// ⛔ THE ts IS A STRING WITH ITS SIX-DIGIT TAIL INTACT. A float round trip
	// destroys that tail and is the single most common Slack integration bug; the
	// fake refuses an update addressed by a mangled ts, so this is load-bearing
	// rather than decorative.
	if !strings.Contains(root.Ref.MessageID, ".") ||
		len(strings.SplitN(root.Ref.MessageID, ".", 2)[1]) != 6 {
		t.Fatalf("the root ts lost its six-digit tail: %q", root.Ref.MessageID)
	}
	// ⛔ THE CONVERSATION COMES FROM THE RESPONSE, NEVER FROM CONFIG (S7).
	if root.Ref.ConversationID != conformanceChannel {
		t.Fatalf("conversation = %q, want the one Slack ANSWERED with (%q)",
			root.Ref.ConversationID, conformanceChannel)
	}
	// A root message is its own thread.
	if root.Ref.ThreadID != root.Ref.MessageID {
		t.Fatalf("a root message must be its own thread root: ts=%q thread=%q",
			root.Ref.MessageID, root.Ref.ThreadID)
	}

	posts := fake.CallsTo("chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("got %d posts for the root, want exactly 1", len(posts))
	}
	if posts[0].ThreadTS != "" {
		t.Errorf("the ROOT was posted with thread_ts=%q — it would be a reply to "+
			"something", posts[0].ThreadTS)
	}
	if posts[0].ReplyBroadcast {
		t.Error("the root card set reply_broadcast; a root message is already in the channel")
	}
	// S6: unfurling is disabled EXPLICITLY, because Slack unfurls by default and
	// a runbook link becoming a preview card spends block budget oto does not own.
	if posts[0].UnfurlLinks != "false" || posts[0].UnfurlMedia != "false" {
		t.Errorf("unfurling was not disabled on the wire: links=%q media=%q",
			posts[0].UnfurlLinks, posts[0].UnfurlMedia)
	}

	// ---- 2. A THREADED REPLY -------------------------------------------
	reply, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "thread_reply_acked"),
		Mode:    chdomain.ModeThreadReply,
		ReplyTo: &root.Ref,
	})
	if err != nil {
		t.Fatalf("post the thread reply: %v", err)
	}
	if reply.Ref.ThreadID != root.Ref.MessageID {
		t.Fatalf("the reply's thread root is %q, want the root ts %q",
			reply.Ref.ThreadID, root.Ref.MessageID)
	}
	if reply.Ref.MessageID == root.Ref.MessageID {
		t.Fatal("the reply reused the root's ts; a reply has a ts of its own")
	}

	posts = fake.CallsTo("chat.postMessage")
	if got := posts[1].ThreadTS; got != root.Ref.MessageID {
		t.Fatalf("the reply threaded off %q, want the ROOT ts %q", got, root.Ref.MessageID)
	}
	if posts[1].ReplyBroadcast {
		t.Error("an ordinary thread reply broadcast into the channel")
	}

	// ---- 3. THE UPDATE, AND IT IS OF THE ROOT --------------------------
	//
	// ⛔ THIS IS THE ASSERTION ADR 0008 LIVES OR DIES ON. A reply now exists and
	// is the most recent message; if the update addressed IT, the channel would
	// show a stale firing card forever while a thread reply quietly changed.
	amended, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_update_acked"),
		Mode:    chdomain.ModeUpdateRoot,
		Target:  &root.Ref,
	})
	if err != nil {
		t.Fatalf("amend the root: %v", err)
	}
	updates := fake.CallsTo("chat.update")
	if len(updates) != 1 {
		t.Fatalf("got %d updates, want 1", len(updates))
	}
	if updates[0].TS != root.Ref.MessageID {
		t.Fatalf("chat.update addressed %q, want the ROOT ts %q (it addressed the "+
			"reply, or a value that has been through a float)", updates[0].TS, root.Ref.MessageID)
	}
	// ⛔ AN EDIT DOES NOT MINT A NEW HANDLE. If the stored ref moved, every
	// subsequent update in the group's life would address the wrong message.
	if amended.Ref.MessageID != root.Ref.MessageID {
		t.Fatalf("the stored ts moved on update: %q → %q", root.Ref.MessageID, amended.Ref.MessageID)
	}

	// ---- 4. THE TERMINAL CARD IS ALSO AN EDIT --------------------------
	if _, err := channel.Amend(ctx, root.Ref, card(t, "root_resolved")); err != nil {
		t.Fatalf("amend the root to its terminal state: %v", err)
	}

	// The shape of the whole episode: ONE post for the root, one for the reply,
	// two edits. ADR 0008's claim is that the second number grows and the first
	// does not.
	if got := len(fake.CallsTo("chat.postMessage")); got != 2 {
		t.Errorf("the episode cost %d posts, want 2 (root + one reply)", got)
	}
	if got := len(fake.CallsTo("chat.update")); got != 2 {
		t.Errorf("the episode cost %d updates, want 2", got)
	}
	if _, _, _, updateCount, ok := fake.Message(conformanceChannel, root.Ref.MessageID); !ok || updateCount != 2 {
		t.Errorf("the fake recorded %d edits of the root, want 2", updateCount)
	}
}

// ⛔ SLACK SAYS "AVOID USING A REPLY'S ts VALUE; USE ITS PARENT INSTEAD" AND DOES
// NOT SAY WHAT HAPPENS IF YOU IGNORE IT.
//
// The provider's `threadRoot` prefers `ThreadID` over `MessageID` for exactly
// this reason. This drives the case that catches a regression: a `ReplyTo` whose
// `MessageID` is a REPLY's ts and whose `ThreadID` is the root — which is
// precisely what the second reply in a thread looks like if a caller stores the
// wrong half of the pair.
func TestOtoNeverThreadsOffAReplyEvenWhenHandedOne(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	ctx := t.Context()

	root, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post the root: %v", err)
	}
	first, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "thread_reply_acked"), Mode: chdomain.ModeThreadReply, ReplyTo: &root.Ref,
	})
	if err != nil {
		t.Fatalf("post the first reply: %v", err)
	}

	// `first.Ref` is a REPLY: MessageID is the reply's own ts, ThreadID is the
	// root's. Threading off it must still use the root.
	if first.Ref.MessageID == first.Ref.ThreadID {
		t.Fatal("the fixture is wrong: the first reply is not distinguishable from a root")
	}
	if _, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "thread_reply_acked"), Mode: chdomain.ModeThreadReply, ReplyTo: &first.Ref,
	}); err != nil {
		t.Fatalf("post a reply threaded off a reply's ref: %v", err)
	}

	for i, c := range fake.CallsTo("chat.postMessage") {
		if c.ThreadTS != "" && c.ThreadTS != root.Ref.MessageID {
			t.Errorf("post %d threaded off %q, which is not the root ts %q",
				i, c.ThreadTS, root.Ref.MessageID)
		}
		// ⚠️ `Flattened` is the fake modelling UNDOCUMENTED behaviour: Slack is
		// reported to re-parent rather than nest. oto must never rely on either
		// reading, and the way to never rely on it is to never send a reply's ts.
		if c.Flattened {
			t.Errorf("post %d was re-parented by Slack — oto threaded off a reply", i)
		}
	}
}

// ---------------------------------------------------------------- ADR 0020

// ADR 0020's mechanism is one parameter, and the whole decision rests on it
// being set on the right messages and only those.
func TestBroadcastIsOneParameterOnAThreadReplyAndIsSetNowhereElse(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	ctx := t.Context()

	root, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post the root: %v", err)
	}
	if _, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "broadcast_refired"),
		Mode:    chdomain.ModeBroadcastReply,
		ReplyTo: &root.Ref,
	}); err != nil {
		t.Fatalf("post the broadcasting re-fire: %v", err)
	}
	// ⛔ THE SECOND BROADCAST WAS `storm_notice` AND ADR 0042 DELETED IT. The claim
	// under test is about the PARAMETER, not about which card carries it: a
	// broadcast must set `reply_broadcast` AND `thread_ts` on every message that
	// uses the mode, and one card sent twice proves that as well as two cards did.
	// ⛔ AND THE CARD CHANGED AGAIN: `broadcast_unacked_reminder` went with the
	// reminder (git-bug bd0fb1d), so `refired` — the only broadcast §H.6 still
	// admits — carries the claim now. The claim itself is unchanged, which is the
	// point of it being about the PARAMETER.
	if _, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "broadcast_refired"),
		Mode:    chdomain.ModeBroadcastReply,
		ReplyTo: &root.Ref,
	}); err != nil {
		t.Fatalf("post the second broadcasting reminder: %v", err)
	}

	posts := fake.CallsTo("chat.postMessage")
	if len(posts) != 3 {
		t.Fatalf("got %d posts, want 3", len(posts))
	}
	if posts[0].ReplyBroadcast {
		t.Error("the root broadcast; `reply_broadcast` is meaningless without thread_ts")
	}
	for i, c := range posts[1:] {
		if !c.ReplyBroadcast {
			t.Errorf("broadcasting reply %d did not set reply_broadcast", i)
		}
		// ⛔ A BROADCAST IS STILL A THREAD REPLY. Slack documents `reply_broadcast`
		// as "used in conjunction with thread_ts"; without one it is silently
		// nothing, and ADR 0020's entire model — thread keeps the detail, channel
		// gets a pointer — collapses into a second root message.
		if c.ThreadTS != root.Ref.MessageID {
			t.Errorf("broadcasting reply %d has thread_ts=%q, want the root %q",
				i, c.ThreadTS, root.Ref.MessageID)
		}
	}

	// ⛔⛔ ADR 0020 RULE 4, ASSERTED ON THE BYTES SLACK RECEIVED. The in-channel
	// reference carries no buttons — that half of Slack's documented claim is the
	// half nobody disputes — so the top-level `text` is very nearly all a channel
	// reader gets, and it has to stand on its own.
	//
	// ⛔ THE MENTION ASSERTIONS WERE HERE AND ARE DELETED (git-bug bd0fb1d). They
	// pinned that the audience appeared in the top-level `text` and NOT inside an
	// attachment — the position being the whole point, because Slack strips
	// attachments from the in-channel reference. The owner withdrew the unacked
	// reminder and ruled the mention goes with it, so nothing emits an audience and
	// there is no position left to defend.
	//
	// ⭐ WHAT REPLACES THEM IS THE STRONGER HALF, AND IT IS BELOW UNCHANGED: the
	// broadcast text must be SELF-SUFFICIENT. That was always the point of rule 4 —
	// a reader in the channel, with no buttons and no attachment, still has to be
	// able to tell what happened. It never depended on anybody being mentioned.
	//
	// ⚠️ THE WORDS MOVED WITH THE CARD, AND THE RULE DID NOT. This probe used to want
	// "unacknowledged", which was the reminder's own word. `refired` is
	// self-sufficient on different evidence — HOW BAD (`Severity critical`), WHAT and
	// WHERE (in the lead), and WHEN it came back (`firing again since`). Checking for
	// the old word against a new card would have been asserting the sentence rather
	// than the property.
	broadcast := posts[1]
	for _, want := range []string{"Severity critical", "firing again since"} {
		if !strings.Contains(broadcast.Text, want) {
			t.Errorf("the broadcast text is not self-sufficient — no %q: %q", want, broadcast.Text)
		}
	}
}

// `chat.update` is documented as accepting `reply_broadcast` — ADR 0020 calls the
// post-quietly-then-broadcast-later path "the sanctioned mechanism" — with one
// hard restriction: `no_dual_broadcast_content_update`, "can't broadcast an old
// reply and update the content at the same time".
//
// oto does not use that path yet. What must hold TODAY is that no ordinary amend
// ever sets the parameter, because an amend always carries content and would earn
// the error on every single edit.
func TestAnAmendNeverCarriesReplyBroadcast(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	ctx := t.Context()

	root, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post the root: %v", err)
	}
	for _, name := range []string{"root_update_acked", "root_silenced", "root_resolved"} {
		if _, err := channel.Amend(ctx, root.Ref, card(t, name)); err != nil {
			t.Fatalf("amend to %s: %v", name, err)
		}
	}
	for i, c := range fake.CallsTo("chat.update") {
		if c.ReplyBroadcast {
			t.Errorf("amend %d set reply_broadcast; with content in the same call that "+
				"is `no_dual_broadcast_content_update` — a terminal, unretryable failure "+
				"on every edit oto makes", i)
		}
	}
}

// ------------------------------------------------- the documented failures

// ⭐ THE FAILURE ADR 0008 IS MOST EXPOSED TO, AND IT IS NOT IN ANY RUNBOOK.
//
// Update-in-place works only while the SAME bot token owns the root. Slack is
// explicit: "only messages posted by the authenticated user are able to be
// updated". A rotated credential, a second oto install pointed at the same
// channel, or a card left by a predecessor app all produce `cant_update_message`
// — and oto's answer must be the thread-pointer recovery, not twelve retries.
func TestUpdatingAMessageAnotherTokenPostedIsTerminalAndRecoverable(t *testing.T) {
	t.Parallel()
	fake := harness.NewSlackConformance(t, conformanceChannel)
	provider := fake.Provider(slackprov.Options{})
	ctx := t.Context()

	open := func(token string) chdomain.Channel {
		kind, values := fake.Credential(token)
		ch, err := provider.Open(ctx, chdomain.ChannelConfig{Raw: fake.Config(conformanceChannel)},
			chdomain.Credential{Kind: kind, Values: values})
		if err != nil {
			t.Fatalf("open with %s: %v", token, err)
		}
		t.Cleanup(func() { _ = ch.Close() })
		return ch
	}

	first := open("xoxb-the-original-token")
	root, err := first.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post the root: %v", err)
	}

	rotated := open("xoxb-the-rotated-token")
	_, err = rotated.Amend(ctx, root.Ref, card(t, "root_resolved"))
	if err == nil {
		t.Fatal("a different token edited the first token's message; Slack does not allow it")
	}
	ce := channelError(t, err)
	if ce.Code != "cant_update_message" {
		t.Fatalf("code = %q, want cant_update_message", ce.Code)
	}
	if ce.Class != chdomain.ClassPermanent {
		t.Fatalf("class = %q, want %q — a rotated token does not become the original "+
			"one on attempt twelve", ce.Class, chdomain.ClassPermanent)
	}
	// §H.9's recovery is keyed off the CODE, not the class: clear the pointer and
	// post a fresh root with a `continued` marker.
	if !slackprov.IsThreadPointerLost(ce.Code) {
		t.Errorf("%q is not classified as a lost thread pointer, so oto would not "+
			"post a continued card and the channel would silently stop updating", ce.Code)
	}
}

// Slack signals rate limiting at the TRANSPORT layer: HTTP 429 with a
// `Retry-After` header in seconds. The header is the only place the deadline
// exists, and honouring it exactly is the difference between backing off and
// being throttled harder.
func TestA429IsClassifiedRateLimitedAndCarriesSlacksOwnDeadline(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	ctx := t.Context()

	fake.Throttle("chat.postMessage", 0, 37)
	_, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err == nil {
		t.Fatal("a 429 produced no error")
	}
	ce := channelError(t, err)
	if ce.Class != chdomain.ClassRateLimited {
		t.Fatalf("class = %q, want %q", ce.Class, chdomain.ClassRateLimited)
	}
	if ce.RetryAfter != 37*time.Second {
		t.Fatalf("RetryAfter = %v, want the 37s Slack asked for. A retry that ignores "+
			"the header is a retry that gets throttled harder", ce.RetryAfter)
	}
}

// Every terminal code oto has an opinion about, driven through the real client
// and the real classifier. ⛔ THE CODES ARE SLACK'S OWN SPELLINGS: a made-up code
// proves nothing about the classification it is supposed to exercise.
func TestTheDocumentedSlackFailuresAreClassifiedByWhatOtoMustDo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code  string
		class chdomain.ErrorClass
		why   string
	}{
		{"channel_not_found", chdomain.ClassPermanent, "the destination is gone; retrying finds nothing"},
		{"not_in_channel", chdomain.ClassPermanent, "the bot was never invited; only a human fixes it"},
		{"is_archived", chdomain.ClassPermanent, "oto has no scope to unarchive anything"},
		{"token_revoked", chdomain.ClassAuthExpired, "the credential is dead and must raise a banner"},
		{"not_allowed_token_type", chdomain.ClassAuthExpired, "an xapp- or xoxp- token where a bot token belongs"},
		{"invalid_blocks", chdomain.ClassConfigInvalid, "oto built an illegal message; it is an oto bug"},
		{"msg_blocks_too_long", chdomain.ClassConfigInvalid, "a payload that is too long is exactly as long on attempt twelve"},
		{"restricted_action_read_only_channel", chdomain.ClassPermanent, "a workspace preference does not change between attempts"},

		// ⚠️ THE FIVE THAT WERE IN NO BUCKET AT ALL AND THEREFORE RETRIED TWELVE
		// TIMES EACH. Every one of them describes a request oto built wrongly, and a
		// request built wrongly is built exactly as wrongly on every attempt.
		{"invalid_metadata_format", chdomain.ClassConfigInvalid, "metadata oto serialised badly"},
		{"invalid_metadata_schema", chdomain.ClassConfigInvalid, "metadata that does not match its declared schema"},
		{"attachment_payload_limit_exceeded", chdomain.ClassConfigInvalid, "the payload is the same size every time"},
		{"invalid_attachments", chdomain.ClassConfigInvalid, "chat.update's own attachment rejection"},
		{"no_dual_broadcast_content_update", chdomain.ClassConfigInvalid,
			"ADR 0020's broadcast-later path earns this the moment it carries content"},

		// ⛔ AND THE ONE THAT WOULD MEAN NO CARD HAS EVER BEEN DELIVERED. Slack lists
		// `metadata_must_be_sent_from_app` on both write methods; oto puts metadata on
		// every card under a bot token. Whether it ever fires is a live-workspace
		// question. What it does WHEN it fires is decidable here, and the answer must
		// be "fail once, loudly, named" rather than "retry twelve times".
		{"metadata_must_be_sent_from_app", chdomain.ClassConfigInvalid,
			"oto attaches metadata to every card under a bot token"},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			fake, channel := newSlackConformanceChannel(t)
			fake.FailNext(tc.code)

			_, err := channel.Deliver(t.Context(), chdomain.DeliverRequest{
				Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
			})
			if err == nil {
				t.Fatalf("%s produced no error", tc.code)
			}
			ce := channelError(t, err)
			if ce.Code != tc.code {
				t.Fatalf("code = %q, want %q verbatim — the code is what the delivery "+
					"record and the health banner show", ce.Code, tc.code)
			}
			if ce.Class != tc.class {
				t.Fatalf("class = %q, want %q: %s", ce.Class, tc.class, tc.why)
			}
			if ce.Class == chdomain.ClassRetryable {
				t.Fatalf("%q retries, and it must not: %s", tc.code, tc.why)
			}
		})
	}
}

// The pessimistic reading of `metadata_must_be_sent_from_app`, driven end to end.
// If it turns out to be the true one, this is what a person sees: not a retry
// storm, not silence — one dead delivery naming the exact code.
func TestIfSlackRefusesMetadataFromABotTokenTheFailureIsLoudAndOnce(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	fake.SetMetadataRequiresAppToken(true)

	msg := card(t, "root_firing")
	if !strings.Contains(string(msg.Payload), `"metadata"`) {
		t.Skip("the root card no longer carries metadata; this risk has gone away")
	}

	_, err := channel.Deliver(t.Context(), chdomain.DeliverRequest{
		Message: msg, Mode: chdomain.ModePostRoot,
	})
	if err == nil {
		t.Fatal("the fake refused the metadata and oto reported success")
	}
	ce := channelError(t, err)
	if ce.Class == chdomain.ClassRetryable {
		t.Fatalf("class = retryable: every card oto sends carries metadata, so this "+
			"would be a permanent retry storm against every alert, not a failure "+
			"anybody could read. code=%q", ce.Code)
	}
}

// ------------------------------------------------- the payloads themselves

// ⭐⭐ THE CLOSED LOOP, BROKEN.
//
// Every card in the corpus is pushed through a SECOND, INDEPENDENT transcription
// of Slack's published Block Kit limits — one that does not import a single
// constant from `internal/channels/render/slack`. oto's own V0–V18 have already
// passed by the time `Render` returns; this asserts that Slack's reference agrees.
//
// It is the closest thing to "Slack accepted it" that exists without a workspace,
// and it is exactly as far as it goes: legality, not legibility.
func TestEveryCardVariantSatisfiesAnIndependentReadingOfSlacksBlockKitRules(t *testing.T) {
	t.Parallel()

	captures, err := harness.RenderSlackCards()
	if err != nil {
		t.Fatalf("render the card corpus: %v", err)
	}
	if len(captures) == 0 {
		t.Fatal("the card corpus is empty")
	}

	for _, c := range captures {
		t.Run(c.Card.Name, func(t *testing.T) {
			t.Parallel()
			fake, channel := newSlackConformanceChannel(t)
			ctx := t.Context()

			root, err := channel.Deliver(ctx, chdomain.DeliverRequest{
				Message: chdomain.RenderedMessage{Fallback: c.Fallback, Payload: c.Wire},
				Mode:    chdomain.ModePostRoot,
			})
			if err != nil {
				t.Fatalf("Slack's own published rules refuse the %s card: %v\n\n%s",
					c.Card.Name, err, c.Wire)
			}

			// The same payload has to survive the verb ADR 0008 makes primary, which
			// has a different error table and a tighter text limit than posting.
			if _, err := channel.Amend(ctx, root.Ref,
				chdomain.RenderedMessage{Fallback: c.Fallback, Payload: c.Wire}); err != nil {
				t.Fatalf("chat.update refuses the %s card: %v", c.Card.Name, err)
			}

			// ⛔ THE BYTES OTO VALIDATED ARE THE BYTES SLACK RECEIVED. The provider
			// passes blocks through verbatim rather than re-marshalling them via the
			// SDK's own types, and this is what proves the passthrough is real: a
			// round trip through another library's structs would mean validating one
			// thing and sending another.
			post := fake.CallsTo("chat.postMessage")[0]
			var sent, rendered struct {
				Attachments []json.RawMessage `json:"attachments"`
			}
			if err := json.Unmarshal(post.Attachments, &sent.Attachments); err != nil {
				t.Fatalf("the attachments Slack received are not decodable: %v", err)
			}
			if err := json.Unmarshal(c.Wire, &rendered); err != nil {
				t.Fatalf("decode the rendered payload: %v", err)
			}
			if len(sent.Attachments) != len(rendered.Attachments) {
				t.Fatalf("oto rendered %d attachments and sent %d",
					len(rendered.Attachments), len(sent.Attachments))
			}
			// The top-level text is the push notification and the only thing a screen
			// reader reads. It must survive to Slack character for character.
			if post.Text != topLevelTextOf(t, c.Wire) {
				t.Fatalf("the top-level text was altered in transit:\n sent: %q\n want: %q",
					post.Text, topLevelTextOf(t, c.Wire))
			}
			if post.Truncated {
				t.Errorf("Slack would silently truncate the %s card's text — a card that "+
					"was shortened without saying so tells an operator a smaller truth "+
					"than the one that exists", c.Card.Name)
			}
		})
	}
}

func topLevelTextOf(t *testing.T, payload json.RawMessage) string {
	t.Helper()
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p.Text
}

// oto's entire Slack surface is three methods. The fake fails any other path, so
// this proves the surface has not grown — which is the thing the one-scope
// manifest depends on and the thing a workspace admin was asked to approve.
func TestOtoTouchesOnlyTheThreeMethodsTheManifestPaysFor(t *testing.T) {
	t.Parallel()
	fake, channel := newSlackConformanceChannel(t)
	ctx := t.Context()

	kind, values := fake.Credential(conformanceToken)
	if _, err := fake.Provider(slackprov.Options{}).VerifyCredential(ctx,
		chdomain.Credential{Kind: kind, Values: values}); err != nil {
		t.Fatalf("auth.test: %v", err)
	}
	root, err := channel.Deliver(ctx, chdomain.DeliverRequest{
		Message: card(t, "root_firing"), Mode: chdomain.ModePostRoot,
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := channel.Amend(ctx, root.Ref, card(t, "root_resolved")); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := channel.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}

	allowed := map[string]bool{"chat.postMessage": true, "chat.update": true, "auth.test": true}
	for _, c := range fake.Calls() {
		if !allowed[c.Method] {
			t.Errorf("oto called %q, which no scope in deploy/slack/manifest.yaml pays for",
				c.Method)
		}
	}
}

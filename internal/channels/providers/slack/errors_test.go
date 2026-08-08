package slack

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// The classification is what drives retry (§F.1). A code in NO bucket falls
// through `classify`'s `default:` and is retried TWELVE TIMES with backoff —
// which is correct for an unknown error and catastrophic for a documented,
// permanent one.

// TestDocumentedPermanentErrorsDoNotRetry covers the three codes that were in no
// map at all, plus the two more found alongside them.
//
// Every string here is verbatim from Slack's chat.postMessage reference.
func TestDocumentedPermanentErrorsDoNotRetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code  string
		class domain.ErrorClass
		why   string
	}{
		// "Cannot post thread replies into a non_threadable channel." A workspace
		// policy, not a transient fault: it does not change between attempt one
		// and attempt twelve.
		{"restricted_action_non_threadable_channel", domain.ClassPermanent, "channel policy"},
		// "Cannot post top-level messages into a thread-only channel."
		{"restricted_action_thread_only_channel", domain.ClassPermanent, "channel policy"},
		// "Cannot post any message into a read-only channel."
		{"restricted_action_read_only_channel", domain.ClassPermanent, "channel policy"},
		// "A workspace preference prevents the authenticated user from posting."
		{"restricted_action", domain.ClassPermanent, "workspace preference"},
		// "Blocks submitted with this message are too long." An oto render bug: a
		// payload that is too long is exactly as long on every attempt.
		{"msg_blocks_too_long", domain.ClassConfigInvalid, "render bug"},
		// The predecessor spelling, no longer listed on chat.postMessage. Kept
		// because retiring a code oto may still be told would silently downgrade a
		// permanent render bug into a retry loop.
		{"msg_too_long", domain.ClassConfigInvalid, "render bug, legacy spelling"},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()

			got := classify(errors.New(tc.code))
			if got == nil {
				t.Fatalf("%s was not classified at all", tc.code)
			}
			if got.Class == domain.ClassRetryable {
				t.Fatalf("%s is RETRYABLE — it will burn twelve attempts against a %s "+
					"that will never succeed", tc.code, tc.why)
			}
			if got.Class != tc.class {
				t.Fatalf("%s classified %q, want %q", tc.code, got.Class, tc.class)
			}
			if got.Code != tc.code {
				t.Fatalf("code %q, want %q carried verbatim — the caller's recovery differs by code",
					got.Code, tc.code)
			}
		})
	}
}

// TestChannelPolicyErrorsDoNotKillTheThread.
//
// They share ClassPermanent with `conversationDead` and `threadPointerLost` but
// NOT their recovery: the conversation is alive and the credential is good, so
// marking the thread dead and degrading the Channel would overstate what Slack
// said. `domain.DeadReasonFor` is what makes that distinction, and it must not
// recognise these codes.
func TestChannelPolicyErrorsDoNotKillTheThread(t *testing.T) {
	t.Parallel()

	for code := range channelPolicyBlocked {
		if IsConversationDead(code) {
			t.Errorf("%s is treated as a dead conversation; the channel still exists", code)
		}
		if IsThreadPointerLost(code) {
			t.Errorf("%s is treated as a lost thread pointer; the pointer is fine", code)
		}
		if IsAuthFailure(code) {
			t.Errorf("%s is treated as an auth failure; the credential is fine", code)
		}
		if IsRenderInvalid(code) {
			t.Errorf("%s is treated as an oto render bug; oto built a legal message", code)
		}
	}
	// The other half of this property — that none of these codes reaches
	// `notification/domain.DeadReasonFor`'s closed set, and so none of them marks
	// a thread dead — is asserted in that module, which owns the set. This package
	// must not import it (§I.3).
}

// TestTheLongestKnownCodeWins.
//
// Slack's codes nest — `restricted_action` is a prefix of
// `restricted_action_thread_locked`, and `invalid_blocks` of
// `invalid_blocks_format`. Scanning the maps in Go's randomised order and taking
// the first `strings.Contains` hit made the reported Code non-deterministic: two
// runs of the same failure could store two different `dead_reason` values, and
// `restricted_action` (permanent, no recovery) could shadow
// `restricted_action_thread_locked` (a state transition with a fresh-root
// recovery).
func TestTheLongestKnownCodeWins(t *testing.T) {
	t.Parallel()

	cases := []struct{ msg, want string }{
		{"slack: post failed: restricted_action_thread_locked", "restricted_action_thread_locked"},
		{"slack: post failed: invalid_blocks_format", "invalid_blocks_format"},
		{"slack: post failed: restricted_action_non_threadable_channel", "restricted_action_non_threadable_channel"},
	}

	for _, tc := range cases {
		// Run it repeatedly: a map-order bug is intermittent by nature and one
		// pass can pass by luck.
		for i := 0; i < 64; i++ {
			if got := knownCodeIn(tc.msg); got != tc.want {
				t.Fatalf("iteration %d: knownCodeIn(%q) = %q, want %q", i, tc.msg, got, tc.want)
			}
		}
	}

	// And the whole classification stays stable through the wrapping.
	err := fmt.Errorf("delivering to #sre: %w", errors.New("restricted_action_thread_locked"))
	for i := 0; i < 64; i++ {
		if got := classify(err); got.Code != "restricted_action_thread_locked" {
			t.Fatalf("iteration %d: wrapped code %q", i, got.Code)
		}
	}
}

// TestBotTokenRefusesAGenericKeyFromAnotherCredentialKind.
//
// `botToken` used to test `cred.Kind != CredBotToken`, write `_ = cred.Kind` and
// fall through — a branch that expressed nothing. The fall-through meant a
// `slack_signing_secret` blob, whose `value` is an HMAC secret, was handed to the
// SDK as a bearer token: an `invalid_auth` that named the wrong credential, and a
// signing secret placed in an Authorization header bound for Slack.
func TestBotTokenRefusesAGenericKeyFromAnotherCredentialKind(t *testing.T) {
	t.Parallel()

	signing := domain.Credential{
		Kind:   CredSigningSecret,
		Values: map[string]string{"value": "8f742231b10e8888abcd99yyyzzz85a5"},
	}
	if got := botToken(signing); got != "" {
		t.Fatalf("a signing secret was returned as a bot token: %q", got)
	}

	// A blob of another kind that EXPLICITLY carries a bot token alongside its own
	// secret is still accepted — that is the case the original comment described,
	// and refusing it would break an install over a labelling detail.
	app := domain.Credential{
		Kind:   CredAppToken,
		Values: map[string]string{"app_token": "xapp-1-A", "bot_token": "xoxb-real"},
	}
	if got := botToken(app); got != "xoxb-real" {
		t.Fatalf("an explicit bot_token on an app-token blob was refused: %q", got)
	}

	// The ordinary cases still work.
	for _, key := range []string{"bot_token", CredBotToken, "token", "value"} {
		cred := domain.Credential{Kind: CredBotToken, Values: map[string]string{key: "xoxb-x"}}
		if got := botToken(cred); got != "xoxb-x" {
			t.Errorf("key %q on a bot-token blob: got %q", key, got)
		}
	}
	// An unlabelled blob is trusted for the generic keys: the Kind is the secret
	// store's business and it is not always populated.
	if got := botToken(domain.Credential{Values: map[string]string{"value": "xoxb-y"}}); got != "xoxb-y" {
		t.Fatalf("an unlabelled blob was refused: %q", got)
	}
}

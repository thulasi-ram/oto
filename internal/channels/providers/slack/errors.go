package slack

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// providerName is the verbatim string carried on every classified error, so a
// support question can be answered without a packet capture.
const providerName = "slack"

// Terminal Slack error codes, grouped by what oto must DO about them (§H.9).
//
// This is the file that decides whether an alert is delivered or lost, so the
// grouping is by consequence, not by Slack's own taxonomy.

// conversationDead means the destination itself is gone. The thread is marked
// dead, the Channel is degraded, and the operator is told. Retrying would burn
// twelve attempts against a channel that will never exist again.
var conversationDead = map[string]bool{
	"channel_not_found": true,
	"is_archived":       true,
	"not_in_channel":    true,
	"is_inactive":       true,
}

// threadPointerLost means the CONVERSATION is fine but oto's stored ts no longer
// addresses anything. This is a STATE TRANSITION, not a failure: clear
// provider_thread_id, post a fresh root with a `continued` marker, re-point the
// thread. Retrying the same ts would fail forever.
var threadPointerLost = map[string]bool{
	"message_not_found":               true,
	"cannot_reply_to_message":         true,
	"restricted_action_thread_locked": true,
	"edit_window_closed":              true,
	"cant_update_message":             true,
	"message_limit_exceeded":          true,
}

// channelPolicyBlocked means the conversation EXISTS, the credential is fine and
// the thread pointer is good — a workspace or channel POLICY refuses this shape
// of message. Slack documents each of these on chat.postMessage:
//
//	restricted_action_non_threadable_channel  "Cannot post thread replies into a
//	                                           non_threadable channel."
//	restricted_action_thread_only_channel     "Cannot post top-level messages into
//	                                           a thread-only channel."
//	restricted_action_read_only_channel       "Cannot post any message into a
//	                                           read-only channel."
//	restricted_action                         "A workspace preference prevents the
//	                                           authenticated user from posting."
//
// ⚠️ NONE of these was in any bucket, so all four fell through `classify`'s
// `default:` and RETRIED TWELVE TIMES against a channel whose administrator has
// decided the answer is no. A workspace preference does not change between
// attempt one and attempt twelve; the retries are pure noise against oto's own
// per-channel posting budget.
//
// They are PERMANENT rather than config_invalid: oto built a legal message and
// the destination refused it, which is a fact about the destination, not a bug in
// the renderer. They are not `conversationDead` either — the conversation is
// alive and a different mode may well succeed in it, so killing the thread and
// degrading the Channel would overstate what Slack said.
var channelPolicyBlocked = map[string]bool{
	"restricted_action_non_threadable_channel": true,
	"restricted_action_thread_only_channel":    true,
	"restricted_action_read_only_channel":      true,
	"restricted_action":                        true,
}

// authFailed means the credential is dead. It NEVER retries, because "your token
// was revoked three days ago and nobody noticed" is a product feature: the
// Channel goes auth_failed and raises a banner (§F.1).
var authFailed = map[string]bool{
	"token_revoked":          true,
	"token_expired":          true,
	"invalid_auth":           true,
	"not_authed":             true,
	"account_inactive":       true,
	"missing_scope":          true,
	"no_permission":          true,
	"not_allowed_token_type": true,
	"org_login_required":     true,
	"ekm_access_denied":      true,
}

// renderInvalid means oto built an illegal message. It is an oto BUG, it is dead
// on arrival, and oto alerts on itself for it (oto_render_invalid_total).
//
// ⚠️ `msg_blocks_too_long` ("Blocks submitted with this message are too long") is
// the spelling Slack's chat.postMessage reference carries TODAY, and it was in no
// bucket at all — so an over-long render retried twelve times, which is twelve
// guaranteed failures because a payload that is too long is exactly as long on
// every attempt. `msg_too_long` is its predecessor: it is no longer listed on
// chat.postMessage, but it is kept here because retiring a code oto may still be
// told by an older workspace would silently downgrade a permanent render bug into
// a retry loop. BOTH are handled; neither is guessed at.
var renderInvalid = map[string]bool{
	"invalid_blocks":         true,
	"invalid_blocks_format":  true,
	"block_mismatch":         true,
	"msg_blocks_too_long":    true,
	"msg_too_long":           true,
	"too_many_attachments":   true,
	"metadata_too_large":     true,
	"no_text":                true,
	"markdown_text_conflict": true,
	"invalid_arguments":      true,
}

// IsConversationDead reports whether code means the destination no longer exists.
// The caller marks channel_threads.state='dead' with this code as dead_reason and
// degrades the Channel — it does NOT retry (§H.9).
func IsConversationDead(code string) bool { return conversationDead[code] }

// IsThreadPointerLost reports whether code means oto's stored message pointer is
// gone. The caller clears provider_thread_id and DEGRADES TO A FRESH ROOT MESSAGE
// with a `continued` marker. It does NOT retry (§H.9).
func IsThreadPointerLost(code string) bool { return threadPointerLost[code] }

// IsAuthFailure reports whether code means the credential is dead. The caller
// sets channels.health_status='auth_failed' and raises a UI banner.
func IsAuthFailure(code string) bool { return authFailed[code] }

// IsChannelPolicyBlocked reports whether code means a workspace or channel POLICY
// refuses this shape of message. The destination is alive and the credential is
// good; there is simply nothing to retry.
func IsChannelPolicyBlocked(code string) bool { return channelPolicyBlocked[code] }

// IsRenderInvalid reports whether code means oto built an illegal message. It is
// an oto bug, and no attempt count will fix it.
func IsRenderInvalid(code string) bool { return renderInvalid[code] }

// classify turns any error the Slack SDK produced into the port's ErrorClass.
//
// THE CLASSIFICATION drives retry, never the provider (§F.1). Everything oto does
// about a failure is decided from the returned Class and Code; nothing downstream
// re-inspects the SDK's error.
func classify(err error) *domain.Error {
	if err == nil {
		return nil
	}

	// Rate limiting first: it is the only class that carries a deadline, and
	// honouring Retry-After EXACTLY is the difference between backing off and
	// being throttled harder.
	var rl *slack.RateLimitedError
	if errors.As(err, &rl) {
		retry := rl.RetryAfter
		if retry <= 0 {
			retry = time.Second
		}
		return &domain.Error{
			Class:      domain.ClassRateLimited,
			RetryAfter: retry,
			Provider:   providerName,
			Code:       "ratelimited",
			Cause:      err,
		}
	}

	var sce slack.StatusCodeError
	if errors.As(err, &sce) {
		switch {
		case sce.Code == http.StatusTooManyRequests:
			return &domain.Error{
				Class:      domain.ClassRateLimited,
				RetryAfter: time.Second,
				Provider:   providerName,
				Code:       "ratelimited",
				Cause:      err,
			}
		case sce.Code == http.StatusUnauthorized || sce.Code == http.StatusForbidden:
			return &domain.Error{
				Class: domain.ClassAuthExpired, Provider: providerName,
				Code: "invalid_auth", Cause: err,
			}
		case sce.Code >= 500:
			return retryable(err, "http_"+http.StatusText(sce.Code))
		default:
			return &domain.Error{
				Class: domain.ClassPermanent, Provider: providerName,
				Code: "http_" + http.StatusText(sce.Code), Cause: err,
			}
		}
	}

	// A timeout or a reset is oto's least interesting failure and its most common
	// one. It retries.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return retryable(err, "timeout")
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return retryable(err, "network")
	}

	code := slackErrorCode(err)
	switch {
	case code == "":
		return retryable(err, "unknown")
	case code == "ratelimited" || code == "rate_limited":
		return &domain.Error{
			Class: domain.ClassRateLimited, RetryAfter: time.Second,
			Provider: providerName, Code: "ratelimited", Cause: err,
		}
	case authFailed[code]:
		return &domain.Error{Class: domain.ClassAuthExpired, Provider: providerName, Code: code, Cause: err}
	case renderInvalid[code]:
		return &domain.Error{Class: domain.ClassConfigInvalid, Provider: providerName, Code: code, Cause: err}
	case conversationDead[code], threadPointerLost[code], channelPolicyBlocked[code]:
		// Permanent, and the caller's recovery differs by code — which is exactly
		// why the Code is carried verbatim rather than collapsed into the class.
		// `channelPolicyBlocked` shares the class and NOT the recovery: it is
		// absent from domain.DeadReasonFor's closed set on purpose, so `fail` does
		// not mark the thread dead over a channel preference the destination may
		// reverse tomorrow.
		return &domain.Error{Class: domain.ClassPermanent, Provider: providerName, Code: code, Cause: err}
	default:
		// Backoff, capped at 12 attempts by the retry policy (§G.6). An unknown
		// Slack error is far more often transient than terminal, and a lost alert
		// is worse than a wasted retry.
		return retryable(err, code)
	}
}

func retryable(err error, code string) *domain.Error {
	return &domain.Error{Class: domain.ClassRetryable, Provider: providerName, Code: code, Cause: err}
}

// slackErrorCode extracts Slack's own error string. The SDK returns it either as
// a SlackErrorResponse or, for most methods, as a bare errors.New(response.Error),
// so both shapes are handled.
func slackErrorCode(err error) string {
	var ser slack.SlackErrorResponse
	if errors.As(err, &ser) && ser.Err != "" {
		return ser.Err
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" || strings.ContainsAny(msg, " \t") {
		// Slack codes are single lower_snake tokens. Anything with whitespace is
		// a transport or SDK message, not a Slack error code.
		return knownCodeIn(msg)
	}
	return msg
}

// knownCodeIn finds a known Slack code embedded in a wrapped error message, so a
// classification is not lost to an fmt.Errorf somewhere up the stack.
//
// ⚠️ THE LONGEST MATCH WINS, and that is not a nicety. Slack's codes nest:
// `restricted_action` is a prefix of `restricted_action_thread_locked`, and
// `invalid_blocks` is a prefix of `invalid_blocks_format`. Iterating maps and
// returning the first `strings.Contains` hit made the reported Code depend on Go's
// randomised map order — two runs of the same failure could store two different
// `dead_reason` values, and `restricted_action` (permanent, no recovery) could
// shadow `restricted_action_thread_locked` (a thread-pointer state transition with
// a fresh-root recovery). Preferring the longest match makes the answer a function
// of the message alone.
func knownCodeIn(msg string) string {
	best := ""
	for _, set := range []map[string]bool{
		conversationDead, threadPointerLost, authFailed, renderInvalid, channelPolicyBlocked,
	} {
		for code := range set {
			if len(code) > len(best) && strings.Contains(msg, code) {
				best = code
			}
		}
	}
	if best != "" {
		return best
	}
	if strings.Contains(msg, "ratelimited") || strings.Contains(msg, "rate limit") {
		return "ratelimited"
	}
	return ""
}

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
var renderInvalid = map[string]bool{
	"invalid_blocks":         true,
	"invalid_blocks_format":  true,
	"block_mismatch":         true,
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
	case conversationDead[code], threadPointerLost[code]:
		// Permanent, and the caller's recovery differs by code — which is exactly
		// why the Code is carried verbatim rather than collapsed into the class.
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
func knownCodeIn(msg string) string {
	for _, set := range []map[string]bool{conversationDead, threadPointerLost, authFailed, renderInvalid} {
		for code := range set {
			if strings.Contains(msg, code) {
				return code
			}
		}
	}
	if strings.Contains(msg, "ratelimited") || strings.Contains(msg, "rate limit") {
		return "ratelimited"
	}
	return ""
}

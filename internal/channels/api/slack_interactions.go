package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
	"github.com/thulasiram/oto/internal/platform/httpx"
	"github.com/thulasiram/oto/internal/platform/log"
)

// Slack's signature contract (§H.8), spelled out because every constant here is
// load-bearing.
const (
	// slackSignatureVersion prefixes the base string and the digest. Slack has
	// versioned this once; pinning it means a future v1 cannot be silently
	// accepted by code written for v0.
	slackSignatureVersion = "v0"
	// slackTimestampHeader carries Unix seconds and is part of the base string, so
	// it cannot be altered without invalidating the signature.
	slackTimestampHeader = "X-Slack-Request-Timestamp"
	// slackSignatureHeader carries `v0=<hex hmac-sha256>`.
	slackSignatureHeader = "X-Slack-Signature"
	// slackReplayWindow is the age beyond which a request is refused. Five minutes
	// is Slack's own recommendation: it defeats replay of a captured request
	// without being so tight that ordinary clock drift breaks interactivity.
	slackReplayWindow = 5 * time.Minute
	// slackMaxBody bounds the form body. The contract caps the `payload` field at
	// 1 MB; the envelope around it is small.
	slackMaxBody int64 = 2 << 20
)

// receiveSlackInteraction serves POST /api/v1/integrations/slack/interactions.
//
// ⛔ THIS IS THE HTTP TRANSPORT AND IT IS UNUSED IN THE DEFAULT DEPLOYMENT.
// Socket Mode is the default for self-hosted oto because it removes the
// public-ingress requirement entirely; HTTP mode is a configuration flag. Both
// transports run behind ONE handler — the port below — so behaviour is identical
// and a bug fixed in one is fixed in both.
//
// ⛔ THE SLACK SDK IS NOT IMPORTED HERE. It lives only in
// `channels/providers/slack`. The signature is verified with `crypto/hmac`
// directly, which also sidesteps the defect that made slack-go < v0.23.1 accept
// an EMPTY signing secret and therefore forged requests: an empty secret here
// disables the endpoint outright.
//
// ⛔ EVERY INTERACTION IS ACKNOWLEDGED WITHIN SLACK'S THREE-SECOND WINDOW,
// including the no-op acknowledgements every URL button still requires. The
// handler therefore answers 200 with an empty body as soon as the envelope is
// authentic, and any work the interaction implies happens on oto's own queues.
// A user must never see "This app is not responding" because a database was slow.
func (rt *Router) receiveSlackInteraction(w http.ResponseWriter, r *http.Request) {
	// A deployment with no signing secret has not enabled HTTP interactivity.
	// Refusing outright is the only safe answer: accepting unverified block
	// actions would let anybody on the internet acknowledge anybody's alert.
	if len(rt.signing) == 0 {
		httpx.WriteProblem(w, r, errs.Unauthorized("slack_signature_unconfigured",
			"this deployment does not accept Slack interactions over HTTP"))
		return
	}

	// The RAW body is required verbatim for verification and must not be
	// re-encoded before it (§H.8). It is read once, bounded, and then parsed from
	// the same bytes that were signed.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, slackMaxBody))
	if err != nil {
		httpx.WriteProblem(w, r, errs.Unauthorized("slack_signature_invalid",
			"the request could not be verified"))
		return
	}

	if err := rt.verifySlack(r, body); err != nil {
		// Every verification failure is reported identically. Distinguishing
		// "stale timestamp" from "bad signature" tells an attacker which half of
		// the forgery to fix.
		log.From(r.Context()).Warn("channels: rejected a Slack interaction",
			"reason", errs.CodeOf(err))
		httpx.WriteProblem(w, r, errs.Unauthorized("slack_signature_invalid",
			"the request could not be verified"))
		return
	}

	payload, err := slackPayload(body)
	if err != nil {
		// The envelope is authentic but unreadable. That is oto's problem, not
		// Slack's user's: acknowledge so the user sees no error, and record it.
		log.From(r.Context()).Error("channels: unreadable Slack interaction payload", "error", err)
		writeSlackAck(w)
		return
	}

	if rt.interactions != nil {
		if err := rt.interactions.Handle(r.Context(), payload); err != nil {
			// ⛔ A handler failure MUST NOT become a non-2xx. Slack retries a
			// non-2xx and shows the user a failure banner; the interaction has
			// already been recorded as received, and oto's own queues own the
			// retry. Logging it is the correct response.
			log.From(r.Context()).Error("channels: Slack interaction handler failed", "error", err)
		}
	}
	writeSlackAck(w)
}

// writeSlackAck sends the empty 200 Slack requires. The body is deliberately
// empty: Slack needs only a prompt 2xx, and anything else would be rendered.
func writeSlackAck(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// verifySlack checks the HMAC over the raw body and the replay window.
//
// The base string is `v0:<timestamp>:<raw body>`, and the comparison is
// CONSTANT TIME — a byte-by-byte comparison of a MAC leaks its own answer through
// timing, which is enough to forge one given patience.
func (rt *Router) verifySlack(r *http.Request, body []byte) error {
	rawTS := strings.TrimSpace(r.Header.Get(slackTimestampHeader))
	if rawTS == "" {
		return errs.Unauthorized("slack_timestamp_missing", "missing request timestamp")
	}
	seconds, err := strconv.ParseInt(rawTS, 10, 64)
	if err != nil {
		return errs.Unauthorized("slack_timestamp_malformed", "malformed request timestamp")
	}

	// The window is checked in BOTH directions. A timestamp far in the future is
	// as suspicious as one far in the past, and only bounding the past would let a
	// captured request be replayed forever by pre-dating it.
	age := rt.now().Sub(time.Unix(seconds, 0).UTC())
	if age > slackReplayWindow || age < -slackReplayWindow {
		return errs.Unauthorized("slack_timestamp_stale", "request timestamp outside the replay window")
	}

	sig := strings.TrimSpace(r.Header.Get(slackSignatureHeader))
	if !strings.HasPrefix(sig, slackSignatureVersion+"=") {
		return errs.Unauthorized("slack_signature_missing", "missing or unversioned signature")
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sig, slackSignatureVersion+"="))
	if err != nil {
		return errs.Unauthorized("slack_signature_malformed", "malformed signature")
	}

	mac := hmac.New(sha256.New, rt.signing)
	mac.Write([]byte(slackSignatureVersion + ":" + rawTS + ":"))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), want) {
		return errs.Unauthorized("slack_signature_mismatch", "signature does not verify")
	}
	return nil
}

// slackPayload extracts the JSON interaction envelope.
//
// Slack posts `application/x-www-form-urlencoded` with a single `payload` field.
// The form is parsed from the bytes that were VERIFIED rather than from
// `r.ParseForm`, because ParseForm consumes the body and would leave the verified
// bytes and the parsed values with no proven relationship.
//
// ⛔ Nothing inside the envelope is trusted as authority. A button's `value` is an
// opaque identifier and is resolved server-side; authority comes from the signed
// envelope, never from the button (§H.8).
func slackPayload(body []byte) (json.RawMessage, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	raw := values.Get("payload")
	if raw == "" {
		return nil, errs.Malformed("slack_payload_missing", "the interaction carried no payload")
	}
	if !json.Valid([]byte(raw)) {
		return nil, errs.Malformed("slack_payload_malformed", "the interaction payload is not JSON")
	}
	return json.RawMessage(raw), nil
}

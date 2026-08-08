package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/thulasiram/oto/internal/platform/errs"
)

// noticeHosts is the exact set of hosts a `response_url` may name.
//
// ⛔⛔ THIS IS AN SSRF CONTROL AND IT IS NOT OPTIONAL. `response_url` arrives
// INSIDE the interaction payload. The payload is authentic — the HMAC proved
// Slack sent it — but "Slack sent it" is not "this URL is safe to POST to", and
// an unchecked URL out of a request body is the textbook shape of a server-side
// request forgery: point it at a metadata service and oto fetches credentials on
// the attacker's behalf.
//
// The list is short because Slack's is. `hooks.slack.com` serves every response
// URL on commercial Slack; `hooks.slack-gov.com` is the GovSlack equivalent.
// Anything else is refused rather than resolved, so no DNS lookup is even made.
var noticeHosts = map[string]bool{
	"hooks.slack.com":     true,
	"hooks.slack-gov.com": true,
}

// noticeTimeout bounds one reply. It is short on purpose: the alert-side outcome
// has already been decided by the time this runs, and a wedged hooks endpoint
// must not hold a job worker.
const noticeTimeout = 5 * time.Second

// noticeMaxBytes bounds the response body oto reads back. Slack answers "ok" and
// nothing else; anything larger is not worth a buffer.
const noticeMaxBytes = 4 << 10

// Notice replies to one interaction on its own `response_url`.
//
// ⭐ IT IS THE ONLY OUTBOUND CALL OTO CAN MAKE IN ANSWER TO A BUTTON, and it is
// the reason "tell the user honestly" is achievable at all. `chat.postEphemeral`
// would need a scope the manifest deliberately does not request; `response_url`
// needs no token, no scope and no bot membership, because Slack mints it for
// this one interaction and expires it.
//
// It does NOT go through the Channel/Provider path, and must not: that path is
// about NOTIFICATIONS — durable intents, threads, ordering, delivery records —
// and this is a transient sentence addressed to one person about a button they
// just pressed. Recording it as a delivery would put a message nobody else can
// see into the audit of what oto told the channel.
type Notice struct {
	client *http.Client
}

// NewNotice builds the reply sender over an HTTP client. A nil client means the
// package default, which is what a deployment with no outbound proxy wants.
func NewNotice(client *http.Client) *Notice {
	if client == nil {
		client = &http.Client{Timeout: noticeTimeout}
	}
	return &Notice{client: client}
}

// noticeBody is Slack's response_url payload.
//
// `replace_original: false` is load-bearing. The default for a block action's
// response URL is to REPLACE the message the button is on — which would delete
// the alert card and put an error sentence in its place. oto's card is the
// record of the alert; a note about a button must never overwrite it.
type noticeBody struct {
	ResponseType    string `json:"response_type"`
	ReplaceOriginal bool   `json:"replace_original"`
	Text            string `json:"text"`
}

// Ephemeral posts one message visible only to the person who pressed the button.
func (n *Notice) Ephemeral(ctx context.Context, responseURL, text string) error {
	if responseURL == "" || text == "" {
		return nil
	}
	u, err := url.Parse(responseURL)
	if err != nil {
		return errs.Validation("slack_response_url_invalid", "the interaction carried an unusable response_url")
	}
	if u.Scheme != "https" || !noticeHosts[u.Hostname()] {
		// Refused BEFORE any resolution or dial. A payload that names another host
		// is either a Slack change worth noticing or an attempt to make oto fetch
		// something on somebody else's behalf, and neither is answered quietly.
		return errs.Validation("slack_response_url_refused",
			"a Slack response_url must be an https URL on a Slack hooks host")
	}

	body, err := json.Marshal(noticeBody{
		ResponseType:    "ephemeral",
		ReplaceOriginal: false,
		Text:            text,
	})
	if err != nil {
		return errs.Internal("slack_notice_encode", err)
	}

	ctx, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return errs.Internal("slack_notice_request", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := n.client.Do(req)
	if err != nil {
		return errs.UpstreamDown("slack_notice_unreachable", "the Slack response URL could not be reached", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained so the connection can be reused, and bounded so a
	// misbehaving endpoint cannot make oto read for as long as it likes.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, noticeMaxBytes))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A response URL expires after 30 minutes and is good for five uses. An
		// expired one is a normal outcome for a job that was retried for a while,
		// and the call site is right to do no more than log it.
		return errs.Newf(errs.KindUpstreamDown, "slack_notice_rejected",
			"the Slack response URL answered %d", resp.StatusCode)
	}
	return nil
}

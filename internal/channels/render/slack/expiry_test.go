package slack_test

import (
	"strings"
	"testing"
	"time"

	"github.com/thulasiram/oto/internal/channels/domain"
)

// expiredView is smokeView after the reaper ended its Case as `reason`.
func expiredView(reason string) *domain.NotificationView {
	v := smokeView()
	ended := upstreamStart.Add(26 * time.Hour)
	v.Reason = "expired"
	v.Group.State = "closed"
	v.Group.FiringCount = 0
	v.Group.ExpiredCount = 2
	v.Alerts[0].State = "expired"
	v.Alerts[1].State = "expired"
	v.Case.State = "expired"
	v.Case.ResolveReason = reason
	v.Case.EndedAt = &ended
	v.RenderedAt = ended.Add(time.Second)
	return v
}

// TestAnExpiredCardSaysWhichExpiryEndedIt — ADR 0056 §4. All three expiries are
// `expired` and none is a resolution, and the card says WHY: "the source went
// silent for a day" and "the source was removed" are different facts with
// different fixes. `timeout` keeps the words it always had, which is what keeps
// every existing golden byte-identical.
func TestAnExpiredCardSaysWhichExpiryEndedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		reason    string
		root      string
		reply     string
		replyText string
	}{
		{"timeout", "oto stopped hearing about this", "oto has not heard about this since", ""},
		{"silent", "its source went silent about this", "its source has said nothing about this since",
			"its source went silent about it"},
		{"source_removed", "its source was removed", "its source was removed, so nothing is left",
			"its source was removed"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()

			root := string(renderView(t, expiredView(tc.reason), domain.ModeUpdateRoot).Payload)
			if !strings.Contains(root, tc.root) {
				t.Fatalf("the expired root card does not say %q:\n%s", tc.root, root)
			}

			reply := renderView(t, expiredView(tc.reason), domain.ModeThreadReply)
			body := string(reply.Payload)
			if !strings.Contains(body, tc.reply) {
				t.Fatalf("the expired reply does not say %q:\n%s", tc.reply, body)
			}
			if !strings.Contains(body, "This is NOT a resolution.") {
				t.Fatalf("an expiry reply must say it is not a resolution:\n%s", body)
			}
			if tc.replyText != "" {
				if text := topLevelText(t, reply.Payload); !strings.Contains(text, tc.replyText) {
					t.Fatalf("the reply's top-level text does not say %q: %q", tc.replyText, text)
				}
			}
		})
	}
}

package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thulasiram/oto/internal/drill/domain"
)

// full is an Artifacts in which every stage has succeeded. Each test below
// damages exactly one thing, which is what makes the failure it asserts on
// attributable.
func full() domain.Artifacts {
	channel := uuid.MustParse("018f0000-0000-7000-8000-00000000c001")
	return domain.Artifacts{
		Batch:              domain.BatchFact{Found: true, Status: "processed", AlertCount: 1},
		Alert:              domain.AlertFact{Found: true, ID: uuid.New(), Key: "ak_x", Synthetic: true, State: "firing"},
		Case:               domain.CaseFact{Found: true, ID: uuid.New(), Seq: 1, State: "firing", RuleSnapshotID: uuid.New()},
		Notification:       domain.NotificationFact{Found: true, ID: uuid.New(), Status: "delivered", Reason: "fired", PolicyName: "all critical"},
		Threads:            []domain.ThreadFact{{ChannelID: channel, ChannelName: "#sre", State: "open", ProviderConversationID: "C1", ProviderThreadID: "1700000000.000100", LastSentSeq: 1}},
		Deliveries:         []domain.DeliveryFact{{ChannelID: channel, ChannelName: "#sre", Status: "sent", Mode: "post_root", ThreadSeq: 1, ProviderMessageID: "1700000000.000100"}},
		RuleLookupPossible: true,
	}
}

func stage(t *testing.T, res domain.Result, name domain.StageName) domain.Stage {
	t.Helper()
	for _, s := range res.Stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("stage %q is missing from the result; every stage must always be reported", name)
	return domain.Stage{}
}

func TestObserveReportsEveryStageAlways(t *testing.T) {
	res := domain.Observe(domain.Artifacts{}, false)
	if len(res.Stages) != len(domain.AllStages()) {
		t.Fatalf("got %d stages, want %d — how far it got is half of what the screen is read for",
			len(res.Stages), len(domain.AllStages()))
	}
	for i, name := range domain.AllStages() {
		if res.Stages[i].Name != name {
			t.Fatalf("stage %d = %q, want %q — the chain must be reported in causal order",
				i, res.Stages[i].Name, name)
		}
	}
}

func TestObserveHappyPath(t *testing.T) {
	res := domain.Observe(full(), false)
	if res.Status != domain.DrillPassed {
		t.Fatalf("status = %q, want passed. failed_stage=%q", res.Status, res.FailedStage)
	}
	if res.FailedStage != "" {
		t.Errorf("failed_stage = %q, want empty on a pass", res.FailedStage)
	}
	if len(res.Destinations) != 1 || res.Destinations[0].Status != "sent" {
		t.Errorf("destinations = %+v, want one sent delivery", res.Destinations)
	}
	// ⛔ AN ASSERTION ON `Destinations[0].Broadcast` STOOD HERE AND IS DELETED. It
	// read "a first notification posts a root; broadcast must be false", and it was
	// the only thing pinning `Broadcast = Mode == "broadcast_reply"` — a derivation
	// whose input value no code can produce now that the thread broadcast mechanism is
	// removed. The field is gone from `domain.Destination`, so the test cannot be
	// retargeted at anything: what remains true is asserted one line up, where `Mode`
	// still carries the provider's own word for how the card landed.
	if res.Destinations[0].Mode != "post_root" {
		t.Errorf("mode = %q, want post_root — a first notification posts a root",
			res.Destinations[0].Mode)
	}
}

// ⭐⭐ THE ANSWER THIS WHOLE FEATURE EXISTS TO GIVE. A brand-new install has
// working credentials, a working renderer and no notification policy: the channel
// test passes, no alert ever arrives, and until now nothing in the product said
// why.
func TestObserveNamesNoPolicyAsThePolicyStage(t *testing.T) {
	a := full()
	a.Notification = domain.NotificationFact{
		Found: true, ID: uuid.New(), Status: "suppressed", SuppressedReason: "no_policy", Reason: "fired",
	}
	a.Threads = nil
	a.Deliveries = nil

	res := domain.Observe(a, false)
	if res.Status != domain.DrillFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if res.FailedStage != domain.StagePolicy {
		t.Fatalf("failed_stage = %q, want %q", res.FailedStage, domain.StagePolicy)
	}
	detail := stage(t, res, domain.StagePolicy).Detail
	if !strings.Contains(detail, "notification policy") {
		t.Errorf("detail = %q; it must name the thing an operator has to change", detail)
	}
	// ⛔ Downstream stages must NOT be reported as failures. One cause, one name.
	for _, name := range []domain.StageName{domain.StageThread, domain.StageOrdering, domain.StageDelivery} {
		if got := stage(t, res, name).Status; got != domain.StatusPending {
			t.Errorf("stage %q = %q after an upstream failure, want pending — "+
				"cascading failures turn a diagnostic into noise", name, got)
		}
	}
}

// ⛔ A CORRECTNESS ALARM, NOT A DELIVERY PROBLEM. The pipeline worked; the
// provenance mark did not survive it, so the drill's alert is about to be counted
// in the hygiene rollup, the dashboard and the alert list.
func TestObserveFailsWhenTheSyntheticMarkDidNotPropagate(t *testing.T) {
	a := full()
	a.Alert.Synthetic = false

	res := domain.Observe(a, false)
	if res.FailedStage != domain.StageIdentity {
		t.Fatalf("failed_stage = %q, want %q", res.FailedStage, domain.StageIdentity)
	}
	if !strings.Contains(stage(t, res, domain.StageIdentity).Detail, "pollute") {
		t.Error("the detail must say the row would pollute statistics — that is the actual harm")
	}
}

// ⛔ `TestObserveFailsWhenTheGroupMarkDidNotPropagate` WAS HERE AND IS DELETED
// (git-bug `7570090`), NOT RETARGETED. It set `a.Group.Synthetic = false` and
// demanded `failed_stage == group`. The behaviour it pinned is genuinely gone rather
// than moved: the mark it watched was `alert_groups.synthetic`, a SECOND copy of the
// provenance mark on a row that no longer exists, and there is no Case-shaped
// equivalent to point it at — `alert_cases` has no `synthetic` column, because a Case
// is reached through its alert and no aggregate counts it on its own.
//
// ⭐ THE PROMISE IT DEFENDED IS STILL DEFENDED, by
// `TestObserveFailsWhenTheSyntheticMarkDidNotPropagate` above: `alerts.synthetic` is
// now the whole mark, and a drill still fails loudly rather than quietly polluting
// every statistic oto reports.

// ⛔ THE STAGE LIST ITSELF LOST A MEMBER, so this asserts the shape of the chain
// rather than any one stage's verdict: `group` sat between `case` and `rule_snapshot`
// and nothing may quietly put a tenth stage back without saying which row it looks at.
func TestObserveReportsNoGroupStage(t *testing.T) {
	res := domain.Observe(full(), false)
	for _, st := range res.Stages {
		if st.Name == "group" {
			t.Fatalf("the chain still reports a `group` stage: %+v — `alert_groups` is "+
				"deleted, so there is no row for it to have looked at", st)
		}
	}
	if got := stage(t, res, domain.StageCase).Detail; !strings.Contains(got, "conversation") {
		t.Errorf("case detail = %q; it must say the Case is the conversation, which is "+
			"what the group stage used to claim for a generation", got)
	}
}

func TestObserveReportsADeadThreadWithItsProviderReason(t *testing.T) {
	a := full()
	a.Threads = []domain.ThreadFact{{
		ChannelID: a.Deliveries[0].ChannelID, ChannelName: "#sre",
		State: "dead", DeadReason: "not_in_channel",
	}}
	a.Deliveries = nil

	res := domain.Observe(a, false)
	if res.FailedStage != domain.StageThread {
		t.Fatalf("failed_stage = %q, want %q", res.FailedStage, domain.StageThread)
	}
	if !strings.Contains(stage(t, res, domain.StageThread).Detail, "not_in_channel") {
		t.Error("the provider's own error CODE is what a support question is answered with")
	}
}

// The FIFO stall is invisible everywhere else in the product. Naming both numbers
// is what turns "nothing arrived" into a diagnosis.
func TestObserveReportsAnOrderingStallWithBothSequences(t *testing.T) {
	a := full()
	a.Threads[0].LastSentSeq = 3
	a.Deliveries[0].ThreadSeq = 7
	a.Deliveries[0].Status = "pending"

	res := domain.Observe(a, false)
	st := stage(t, res, domain.StageOrdering)
	if st.Status != domain.StatusPending {
		t.Fatalf("ordering = %q, want pending while the gate holds it", st.Status)
	}
	if st.Facts["thread_seq"] != "7" || st.Facts["last_sent_seq"] != "3" {
		t.Errorf("facts = %v, want both sequence numbers", st.Facts)
	}
}

// ⭐ `skipped`, not `failed`. A drill's alert matches no Prometheus rule because
// oto did not write one in anybody's cluster; a red cross here would send an
// operator hunting a problem that does not exist.
func TestObserveSkipsTheRuleSnapshotHonestly(t *testing.T) {
	a := full()
	a.Case.RuleSnapshotID = uuid.Nil
	a.RuleLookupPossible = false

	res := domain.Observe(a, false)
	st := stage(t, res, domain.StageRuleSnapshot)
	if st.Status != domain.StatusSkipped {
		t.Fatalf("rule_snapshot = %q, want skipped", st.Status)
	}
	if res.Status != domain.DrillPassed {
		t.Fatalf("status = %q — a skipped stage must not block a pass", res.Status)
	}
	if !strings.Contains(st.Detail, "Prometheus") {
		t.Errorf("detail = %q; it must explain why there was nothing to capture", st.Detail)
	}
}

// ⭐ A DIFFERENT VERDICT FROM `failed`, on purpose. "Slack rejected the card" and
// "nothing has picked the job up" send an operator to different places.
func TestObserveTimesOutRatherThanFailing(t *testing.T) {
	a := domain.Artifacts{Batch: domain.BatchFact{Found: true, Status: "pending"}}
	res := domain.Observe(a, true)
	if res.Status != domain.DrillTimedOut {
		t.Fatalf("status = %q, want timed_out", res.Status)
	}
	if res.FailedStage != "" {
		t.Errorf("failed_stage = %q — a timeout names no stage, because none failed", res.FailedStage)
	}
}

// oto's own payload being rejected by oto's own bounds is an oto bug, and the
// screen has to say so rather than implying the operator misconfigured something.
func TestObserveReportsAnIngestRejectionAsOtosOwnBug(t *testing.T) {
	a := full()
	a.Rejections = []domain.RejectionFact{{Reason: "labelset_too_large", Detail: "9 KiB"}}

	res := domain.Observe(a, false)
	if res.FailedStage != domain.StageProcess {
		t.Fatalf("failed_stage = %q, want %q", res.FailedStage, domain.StageProcess)
	}
	if !strings.Contains(stage(t, res, domain.StageProcess).Detail, "oto bug") {
		t.Error("the detail must blame oto, not the operator")
	}
}

func TestObserveFailsWhenEveryDeliveryIsTerminal(t *testing.T) {
	a := full()
	a.Deliveries[0].Status = "dead"
	a.Deliveries[0].Error = "slack: channel_not_found"
	a.Deliveries[0].ErrorClass = "permanent"

	res := domain.Observe(a, false)
	if res.FailedStage != domain.StageDelivery {
		t.Fatalf("failed_stage = %q, want %q", res.FailedStage, domain.StageDelivery)
	}
	if !strings.Contains(stage(t, res, domain.StageDelivery).Detail, "channel_not_found") {
		t.Error("the provider's code belongs in the detail")
	}
}

func TestSummariseTakesTheFirstFailure(t *testing.T) {
	res := domain.Summarise([]domain.Stage{
		{Name: domain.StageAccept, Status: domain.StatusPassed},
		{Name: domain.StageProcess, Status: domain.StatusFailed},
		{Name: domain.StageIdentity, Status: domain.StatusFailed},
	}, false)
	if res.FailedStage != domain.StageProcess {
		t.Fatalf("failed_stage = %q, want the EARLIEST failure %q",
			res.FailedStage, domain.StageProcess)
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, s := range []domain.Status{domain.DrillPassed, domain.DrillFailed, domain.DrillTimedOut} {
		if !s.Terminal() {
			t.Errorf("%q must be terminal", s)
		}
	}
	if domain.DrillRunning.Terminal() {
		t.Error("running must not be terminal")
	}
}

func TestNewStatusRefusesAnythingOutsideTheCheck(t *testing.T) {
	for _, s := range []string{"running", "passed", "failed", "timed_out"} {
		if _, err := domain.NewStatus(s); err != nil {
			t.Errorf("NewStatus(%q): %v", s, err)
		}
	}
	if _, err := domain.NewStatus("succeeded"); err == nil {
		t.Error("NewStatus accepted a value drills_status_ck would refuse")
	}
}

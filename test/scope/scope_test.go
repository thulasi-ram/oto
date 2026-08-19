package scope

import (
	"testing"

	"github.com/thulasiram/oto/test/harness"
)

// TestMain owns the one Postgres container this binary uses.
//
// ⛔ IT SAID "TWO OF THE THREE GATES" UNTIL git-bug bd0fb1d. The third was AC-52,
// the pure compile-time gate that held `unacked_reminder_after_s` to a scalar in
// every layer; the reminder is withdrawn and the gate was deleted rather than
// weakened, because a type gate over a field that does not exist goes green for
// the wrong reason. Both surviving gates need a real database.
func TestMain(m *testing.M) { harness.Main(m) }

// ⛔ `repoRoot` WAS HERE AND IS DELETED WITH ITS ONLY CALLER (git-bug bd0fb1d).
// It walked up to the module root so AC-52's AST sweep could find `internal/`
// regardless of where `go test` ran from. That sweep is gone; the two remaining
// gates read the database, not the source tree.

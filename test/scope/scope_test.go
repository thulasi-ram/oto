package scope

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thulasiram/oto/test/harness"
)

// TestMain owns the one Postgres container this binary uses. Two of the three
// gates need a real database; the third (AC-52) is pure and runs under `-short`
// like any other compile-time assertion.
func TestMain(m *testing.M) { harness.Main(m) }

// repoRoot walks up from the test's working directory to the module root, so the
// AST sweep in the AC-52 gate can find `internal/` regardless of where `go test`
// was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

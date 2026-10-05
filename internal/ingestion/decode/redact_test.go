package decode

import "testing"

// TestMatchesNameIsTheIngestDialectOverBothLists — the investigator redacts a
// ToolServer's results with this matcher (git-bug 2e9a086), so it must answer exactly as
// the envelope walk does: either list, glob or literal, case and all.
func TestMatchesNameIsTheIngestDialectOverBothLists(t *testing.T) {
	r := NewRedactor([]string{"*password*", " "}, []string{"api_token", "["})
	for name, want := range map[string]bool{
		"db_password": true, "api_token": true, "DB_PASSWORD": false, "token": false, "[": true,
	} {
		if got := r.MatchesName(name); got != want {
			t.Errorf("MatchesName(%q) = %v, want %v", name, got, want)
		}
	}
	if NewRedactor(nil, nil).MatchesName("password") {
		t.Error("an empty Redactor matched")
	}
}

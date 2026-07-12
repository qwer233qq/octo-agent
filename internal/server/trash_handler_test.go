package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-octo/octo-agent/internal/trash"
)

// stageConflict trashes a file, then recreates a different file at the original
// path, so a restore has to deal with an occupied destination. Returns the
// entry id and the original path.
func stageConflict(t *testing.T) (string, string) {
	t.Helper()
	project := t.TempDir()
	orig := filepath.Join(project, "notes.txt")
	if err := os.WriteFile(orig, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := trash.Move(orig, project, trash.Options{DeletedBy: "session", Kind: "delete"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orig, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := trash.List()
	if len(entries) != 1 {
		t.Fatalf("expected 1 trashed entry, got %d", len(entries))
	}
	return entries[0].ID, orig
}

// TestRestoreTrash_ConflictAbortThenBackup: the default restore 409s on an
// occupied path; on_conflict=backup resolves it losslessly.
func TestRestoreTrash_ConflictAbortThenBackup(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	id, orig := stageConflict(t)

	if w := doJSON(t, srv, "POST", "/api/trash/"+id+"/restore", ""); w.Code != 409 {
		t.Fatalf("expected 409 on conflict, got %d (body %s)", w.Code, w.Body.String())
	}
	// The current file is untouched by the aborted restore.
	if b, _ := os.ReadFile(orig); string(b) != "new" {
		t.Fatalf("aborted restore must not touch the current file, got %q", b)
	}

	w := doJSON(t, srv, "POST", "/api/trash/"+id+"/restore?on_conflict=backup", "")
	if w.Code != 200 {
		t.Fatalf("on_conflict=backup should succeed, got %d (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		BackedUpExisting bool `json:"backed_up_existing"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.BackedUpExisting {
		t.Error("expected backed_up_existing=true")
	}
	if b, _ := os.ReadFile(orig); string(b) != "old" {
		t.Fatalf("restore should have put the old version back, got %q", b)
	}
}

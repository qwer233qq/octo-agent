package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-octo/octo-agent/internal/trash"
)

// TestWindowsSafeRm_EndToEnd drives the real PowerShell Remove-Item wrapper
// against a freshly built octo binary: a delete must both remove the file AND
// leave it recoverable in the trash, for a relative and an absolute path. It is
// the Windows counterpart of TestSafeRm_AbsolutePathBackedUp.
//
// Windows-only: the wrapper shadows the Windows PowerShell Remove-Item cmdlet
// and shells out to `octo __trash-backup`. It builds octo with `go build`, so
// it's a slower integration test guarded to the platform it protects.
func TestWindowsSafeRm_EndToEnd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows Remove-Item wrapper only")
	}
	ps := resolvePowerShell()
	if _, err := exec.LookPath(ps); err != nil {
		t.Skipf("no PowerShell (%s) available", ps)
	}

	// Build the octo binary the wrapper calls back into.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	exe := filepath.Join(t.TempDir(), "octo.exe")
	build := exec.Command("go", "build", "-o", exe, "github.com/open-octo/octo-agent/cmd/octo")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build octo: %v\n%s", err, out)
	}

	workdir := t.TempDir()
	rel := filepath.Join(workdir, "rel.txt")
	abs := filepath.Join(t.TempDir(), "abs.txt") // absolute, outside workdir
	for _, p := range []string{rel, abs} {
		if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Mirror shellCommand's wiring: %s = octo exe (single-quote escaped), then
	// the user command. -Force ghost.txt must be ignored (non-existent path and
	// a -flag), proving only real filesystem paths are staged.
	command := "Remove-Item rel.txt; Remove-Item '" + abs + "'; Remove-Item -Force ghost.txt"
	wrapped := fmt.Sprintf(windowsSafeRmWrapper, strings.ReplaceAll(exe, "'", "''"), command)
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", wrapped)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "OCTO_TRASH_PROJECT="+workdir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper run: %v\n%s", err, out)
	}

	// Both targets deleted by the real cmdlet.
	for _, p := range []string{rel, abs} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been deleted", p)
		}
	}

	// Both recoverable, stamped rm/delete; the ghost was never staged.
	entries, err := trash.List()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]trash.Entry{}
	for _, e := range entries {
		seen[e.Original] = e
	}
	for _, p := range []string{rel, abs} {
		e, ok := seen[p]
		if !ok {
			t.Errorf("%s not backed up", p)
			continue
		}
		if e.DeletedBy != "rm" || e.Kind != "delete" {
			t.Errorf("%s provenance = %q/%q, want rm/delete", p, e.DeletedBy, e.Kind)
		}
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Original, "ghost.txt") {
			t.Error("a non-existent target must not be staged")
		}
	}
}

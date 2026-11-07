package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The board's core promise: a harness can move work up to review, only a human closes it.
func TestMoveGate(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("KANBAN_DATA", dir)
	db = openDB()
	defer os.Setenv("KANBAN_DATA", "")

	db.Exec(`INSERT INTO apps (id,name) VALUES ('app','app')`)
	i := addIssue("app", Issue{Title: "bug", Plan: "scoped", Status: "plan_ready"})
	if i.Status != "plan_ready" {
		t.Fatalf("new issue status = %q, want plan_ready", i.Status)
	}

	if _, err := patchIssue(itoa(i.ID), Issue{Status: "fixing"}, false); err != nil {
		t.Fatalf("harness should start work: %v", err)
	}
	if _, err := patchIssue(itoa(i.ID), Issue{Status: "review", ReviewNotes: "done"}, false); err != nil {
		t.Fatalf("harness should submit for review: %v", err)
	}
	if _, err := patchIssue(itoa(i.ID), Issue{Status: "done"}, false); err == nil {
		t.Fatal("harness must not be able to set done")
	}
	if _, err := patchIssue(itoa(i.ID), Issue{Status: "done"}, true); err != nil {
		t.Fatalf("human should be able to set done: %v", err)
	}
	if _, err := patchIssue(itoa(i.ID), Issue{Status: "nonsense"}, true); err == nil {
		t.Fatal("unknown status must be rejected")
	}
}

func TestSyncProjectsLinksRealFolders(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("KANBAN_DATA", dir)
	db = openDB()

	root := t.TempDir()
	for _, name := range []string{"Alpha-App", "Beta"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	added := syncProjects(root)
	if len(added) != 2 {
		t.Fatalf("linked %d projects, want 2 (%v)", len(added), added)
	}
	var path, source string
	if err := db.QueryRow(`SELECT repo_path, source FROM apps WHERE id='alpha_app'`).Scan(&path, &source); err != nil {
		t.Fatalf("alpha_app missing: %v", err)
	}
	if path != filepath.Join(root, "Alpha-App") || source != "repo" {
		t.Fatalf("alpha_app row = %q/%q", path, source)
	}
	// re-running must be idempotent
	if again := syncProjects(root); len(again) != 0 {
		t.Fatalf("second sync added %v, want none", again)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

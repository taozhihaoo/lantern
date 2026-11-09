package scan

import (
	"strings"
	"testing"
)

// stateOpts builds Options with an in-memory index state table.
func stateOpts(base string, targets []string, maxSize int64, st map[string]StateEntry) Options {
	return Options{
		Base: base, Targets: targets, Workers: 1, MaxSize: maxSize,
		State: func(p string) (StateEntry, bool) {
			e, ok := st[p]
			return e, ok
		},
		StateList: func() []string {
			out := make([]string, 0, len(st))
			for p := range st {
				out = append(out, p)
			}
			return out
		},
	}
}

// P0 regression: when every target is unreadable (deleted directory, typo,
// permission error), the old code produced an empty `seen` set and the
// deletion pass wiped the whole index while still reporting "index complete".
func TestUnreadableTargetDoesNotWipeIndex(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, "a.md", "alpha")
	st := map[string]StateEntry{"a.md": {Size: 5, MTime: 1, SHA: "abc"}}

	_, deletes, _, err := Run(stateOpts(base, []string{"no-such-dir"}, 1<<20, st))
	if err == nil {
		t.Fatal("expected an error when no target can be read")
	}
	if len(deletes) != 0 {
		t.Fatalf("documents must not be deleted: %v", deletes)
	}
}

// P1 regression: a file that still exists on disk but is skipped (too large,
// ignored, transient stat error) must not be treated as deleted.
func TestSkippedExistingFileNotDeleted(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, "big.md", strings.Repeat("x", 200))
	st := map[string]StateEntry{"big.md": {Size: 200, MTime: 1, SHA: "abc"}}

	_, deletes, sum, err := Run(stateOpts(base, []string{"."}, 50, st))
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 0 {
		t.Fatalf("existing but skipped file was deleted: %v", deletes)
	}
	if sum.SkipReasonCount(SkipTooLarge) != 1 {
		t.Fatalf("expected too_large skip, got %v", sum.Skipped)
	}
}

// Deletion detection must be scoped to roots that were actually walked.
func TestPartialTargetFailureScopesDeletion(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, "keep/a.md", "alpha")
	st := map[string]StateEntry{
		"keep/a.md": {Size: 5, MTime: 1, SHA: "abc"},
		"gone/b.md": {Size: 5, MTime: 1, SHA: "def"},
	}
	_, deletes, _, err := Run(stateOpts(base, []string{"keep", "gone"}, 1<<20, st))
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 0 {
		t.Fatalf("unreadable root must not cause deletions: %v", deletes)
	}
}

// A legitimate removal inside a readable root must still be detected.
func TestRealDeletionStillDetected(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, "keep.md", "alpha")
	st := map[string]StateEntry{
		"keep.md": {Size: 5, MTime: 1, SHA: "abc"},
		"gone.md": {Size: 5, MTime: 1, SHA: "def"},
	}
	_, deletes, _, err := Run(stateOpts(base, []string{"."}, 1<<20, st))
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 || deletes[0] != "gone.md" {
		t.Fatalf("deletes = %v, want [gone.md]", deletes)
	}
}

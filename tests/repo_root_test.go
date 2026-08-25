package sofab_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot returns the module root directory.
//
// A few black-box tests open files kept at the root — README-adjacent docs, the
// assets/ vectors, the bench/ scripts, .devcontainer/. Now that the suite lives
// under tests/, go test sets the working directory to tests/, so a bare
// relative path would miss. Anchoring to this file's own location instead of the
// working directory keeps those reads correct wherever the suite is invoked
// from; walking up to go.mod avoids hard-coding how deep tests/ sits.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate the test source file")
	}
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repoRoot: no go.mod found above %s", filepath.Dir(self))
		}
		dir = parent
	}
}

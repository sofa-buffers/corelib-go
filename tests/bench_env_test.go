package sofab_test

// Environment regression tests for the benchmark tooling (CORELIB_PLAN §10,
// §13). §10 requires this repo to ship three benchmark tools — `perf`, `bench`
// and `bench/run_callgrind.sh` — and the §13 conformance checklist requires
// them to be *runnable*, not merely present. BENCH_SPEC has the central harness
// build each implementation in its own `.devcontainer`, so the dev container is
// the declared environment those tools have to be runnable in; a prereq that
// only happens to exist on a developer's host is not installed at all as far as
// the checklist is concerned.
//
// That makes the link between "what the bench scripts require" and "what
// `.devcontainer/Dockerfile` installs" a contract, and this file tests it:
// issue #97 (the Dockerfile never installed valgrind, so `run_callgrind.sh` and
// `profile.sh` both aborted at their prereq guard inside a freshly built image)
// fails here. The Docker build itself is not exercised — CI has no image to
// build — but the missing package is exactly what a static check catches.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const dockerfilePath = ".devcontainer/Dockerfile"

// benchToolAptPackage maps an external command the bench tooling invokes to the
// Debian/Ubuntu package the dev container must install to provide it. Both
// Callgrind commands ship in `valgrind`, so one package covers both scripts.
//
// Adding a prereq to a script means adding its row here *and* installing it in
// `.devcontainer/Dockerfile`; TestBenchPrereqGuardsHaveAKnownProvider below
// fails on a guard whose command has no row.
var benchToolAptPackage = map[string]string{
	"valgrind":           "valgrind",
	"callgrind_annotate": "valgrind",
}

// prereqGuard matches the `command -v <tool>` prereq guards the bench scripts
// open with.
var prereqGuard = regexp.MustCompile(`command -v ([A-Za-z0-9_.+-]+)`)

// benchScripts returns the shell tools under bench/, keyed by path.
func benchScripts(t *testing.T) map[string]string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(repoRoot(t), "bench", "*.sh"))
	if err != nil {
		t.Fatalf("glob bench/*.sh: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("bench/: no shell tools found; CORELIB_PLAN §10 requires run_callgrind.sh here")
	}

	scripts := make(map[string]string, len(paths))
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		scripts[path] = string(src)
	}
	return scripts
}

// dockerfileAptPackages returns the package names `.devcontainer/Dockerfile`
// hands to `apt-get install`, across every layer. Line continuations are joined
// first, and a package list ends at the next `&&`-joined command.
func dockerfileAptPackages(t *testing.T) map[string]bool {
	t.Helper()

	src := readDoc(t, dockerfilePath)
	joined := strings.ReplaceAll(src, "\\\n", " ")

	pkgs := map[string]bool{}
	for _, line := range strings.Split(joined, "\n") {
		rest := line
		for {
			i := strings.Index(rest, "apt-get install")
			if i < 0 {
				break
			}
			rest = rest[i+len("apt-get install"):]
			for _, field := range strings.Fields(rest) {
				if strings.HasPrefix(field, "-") {
					continue // a flag, e.g. -y / --no-install-recommends
				}
				if field == "&&" || field == ";" || field == "|" || strings.HasPrefix(field, ">") {
					break // the package list ended; the next command starts here
				}
				pkgs[field] = true
			}
		}
	}
	if len(pkgs) == 0 {
		t.Fatalf("%s: no apt-get install package list found; the parser is broken", dockerfilePath)
	}
	return pkgs
}

// requiredBenchTools returns the external commands the bench tooling needs: the
// ones behind a `command -v` guard, plus the known tools a script invokes
// directly (profile.sh calls `callgrind_annotate` without guarding it).
func requiredBenchTools(t *testing.T) map[string][]string {
	t.Helper()

	users := map[string][]string{}
	for path, src := range benchScripts(t) {
		for _, m := range prereqGuard.FindAllStringSubmatch(src, -1) {
			users[m[1]] = appendOnce(users[m[1]], path)
		}
		for tool := range benchToolAptPackage {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(tool) + `\b`).MatchString(src) {
				users[tool] = appendOnce(users[tool], path)
			}
		}
	}
	if len(users) == 0 {
		t.Fatal("bench/: no external tool requirements discovered; the scanner is broken")
	}
	return users
}

func appendOnce(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}

// §10/§13: every external command the bench tooling needs must be installed in
// the dev container, or the tool is not runnable in its declared environment.
func TestBenchPrereqsAreInstalledInTheDevContainer(t *testing.T) {
	installed := dockerfileAptPackages(t)

	for tool, users := range requiredBenchTools(t) {
		pkg, known := benchToolAptPackage[tool]
		if !known {
			continue // reported by TestBenchPrereqGuardsHaveAKnownProvider
		}
		if !installed[pkg] {
			sort.Strings(users)
			t.Errorf("%s never installs %q, so %s (needed by %s) is missing in the dev container: "+
				"CORELIB_PLAN §13 requires the §10 bench tools to be runnable there",
				dockerfilePath, pkg, tool, strings.Join(users, ", "))
		}
	}
}

// A new prereq guard must come with a row in benchToolAptPackage, otherwise the
// test above silently stops covering the script that grew it.
func TestBenchPrereqGuardsHaveAKnownProvider(t *testing.T) {
	for path, src := range benchScripts(t) {
		for _, m := range prereqGuard.FindAllStringSubmatch(src, -1) {
			if _, known := benchToolAptPackage[m[1]]; !known {
				t.Errorf("%s guards on %q, which has no row in benchToolAptPackage: "+
					"add the package that provides it and install it in %s",
					path, m[1], dockerfilePath)
			}
		}
	}
}

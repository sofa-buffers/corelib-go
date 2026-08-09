package sofab_test

// Documentation regression tests for the README's *shape* (CORELIB_PLAN §9).
// §9 fixes one structure for the whole family — "do not change the section
// ordering and do not invent new top-level sections" — so a reader who knows
// one port's README can navigate this one by position. That makes the set of
// `## ` headings a contract rather than an editorial choice, and this file
// tests it: an eighth chapter (issue #96 grew a top-level `## Feature flags`
// between §9.6 and §9.7) fails here.
//
// The remaining tests guard the two ways a fix could go wrong: dropping content
// the spec requires (§6.4 obliges a byte-container port to document its
// strict-UTF-8 knobs) and leaving the in-README links pointing at a heading
// that moved.

import (
	"regexp"
	"strings"
	"testing"
)

// readmeTopLevelSections is the sanctioned set, in order: §9.2, §9.3, §9.5,
// §9.6, §9.7, §9.8. (§9.1 is the header block, whose `# SofaBuffers` is the
// document title; §9.4 forbids an API-documentation chapter.) Anything else at
// `## ` level is an invented section — demote it to a `###` subsection of the
// chapter it belongs to instead of adding a row here.
var readmeTopLevelSections = []string{
	"SofaBuffers Go library",
	"Why this design",
	"Usage",
	"Memory handling",
	"Build & test",
	"Benchmarks",
}

// readmeHeading is a Markdown ATX heading; the level and the text are captured.
var readmeHeading = regexp.MustCompile(`^(#{1,6}) +(.*?)\s*$`)

type mdHeading struct {
	level int
	text  string
}

// readmeHeadings parses README.md's headings, skipping fenced code blocks so a
// shell comment inside an example is never mistaken for a chapter.
func readmeHeadings(t *testing.T) []mdHeading {
	t.Helper()

	var headings []mdHeading
	fenced := false
	for _, line := range strings.Split(readDoc(t, "README.md"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := readmeHeading.FindStringSubmatch(line); m != nil {
			headings = append(headings, mdHeading{level: len(m[1]), text: m[2]})
		}
	}
	if len(headings) == 0 {
		t.Fatal("README.md: no headings found; the parser is broken")
	}
	return headings
}

// §9: the top-level sections are fixed, in both membership and order.
func TestReadmeTopLevelSectionsMatchThePlan(t *testing.T) {
	var got []string
	for _, h := range readmeHeadings(t) {
		if h.level == 2 {
			got = append(got, h.text)
		}
	}

	want := readmeTopLevelSections
	sanctioned := map[string]bool{}
	for _, name := range want {
		sanctioned[name] = true
	}
	present := map[string]bool{}
	for _, name := range got {
		present[name] = true
		if !sanctioned[name] {
			t.Errorf("README.md: invented top-level section %q — §9: \"do not invent new top-level sections\"; demote it to a `###` subsection of the chapter it belongs to", name)
		}
	}
	for _, name := range want {
		if !present[name] {
			t.Errorf("README.md: missing top-level section %q (CORELIB_PLAN §9)", name)
		}
	}
	if t.Failed() {
		return
	}
	// Membership matches; only the ordering can still differ.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("README.md: top-level section %d is %q, want %q — §9 fixes the section order", i+1, got[i], want[i])
		}
	}
}

// §9.4: the Docs badge is the single entry point to the API reference, at any
// heading level — demoting such a chapter to `###` would not make it allowed.
func TestReadmeHasNoAPIDocumentationChapter(t *testing.T) {
	forbidden := []string{"api reference", "api documentation", "source documentation"}
	for _, h := range readmeHeadings(t) {
		title := strings.ToLower(h.text)
		for _, bad := range forbidden {
			if title == bad {
				t.Errorf("README.md: %q heading %q — §9.4 forbids an API-documentation chapter; the Docs badge is the only pointer", strings.Repeat("#", h.level), h.text)
			}
		}
	}
}

// §6.4 obliges a byte-container port to document the strict-UTF-8 option, its
// default, and the build that compiles the validator out. Rearranging the
// README must not lose any of that: the knobs stay documented, wherever they
// live.
func TestReadmeDocumentsTheStrictUTF8Knobs(t *testing.T) {
	text := readDoc(t, "README.md")
	for _, knob := range []string{"SOFAB_STRICT_UTF8", "WithStrictUTF8", "sofab_no_strict_utf8", "WithPassThrough"} {
		if !strings.Contains(text, knob) {
			t.Errorf("README.md: never documents %s (CORELIB_PLAN §6.4/§5.1)", knob)
		}
	}
}

// A heading that moves takes its anchor with it, so every in-document link must
// still resolve — the cheapest way for a restructuring to break navigation.
func TestReadmeInternalLinksResolve(t *testing.T) {
	anchors := map[string]bool{}
	for _, h := range readmeHeadings(t) {
		anchors[githubAnchor(h.text)] = true
	}
	links := regexp.MustCompile(`\]\(#([^)]+)\)`).FindAllStringSubmatch(readDoc(t, "README.md"), -1)
	if len(links) == 0 {
		t.Fatal("README.md: no in-document links found; the scan is broken")
	}
	for _, l := range links {
		if !anchors[l[1]] {
			t.Errorf("README.md: link to #%s matches no heading", l[1])
		}
	}
}

// githubAnchor slugifies a heading the way GitHub does: lowercase, punctuation
// dropped, spaces to hyphens.
func githubAnchor(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

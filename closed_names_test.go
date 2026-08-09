package sofab

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CORELIB_PLAN §6.1.1 closes the name set of the generated-object layer to
// encode / decode / try_decode / serialize / deserialize / decoder, and lists the
// spellings a port must not invent for it. This package's docs describe that
// layer (it is what drives the runtime), so describing it in a forbidden
// spelling teaches the wrong surface just as effectively as emitting it: a
// reader looking for Marshal/Unmarshal on a generated type finds nothing, since
// the Go backend emits Serialize / Encode / EncodeTo / Decode<T> / Decode<T>From.
//
// The scan therefore covers the whole documentation surface — README.md and
// every .go file, whose comments and examples become the godoc page.
var forbiddenGeneratedNames = []string{
	"marshal",
	"unmarshal",
	"serialize_to",
	"to_bytes",
	"from_bytes",
	"decode_from",
	"decode_into",
}

// scannerFile is this file, which necessarily spells the forbidden words out to
// look for them.
const scannerFile = "closed_names_test.go"

func TestDocsUseOnlyTheClosedGeneratedNameSet(t *testing.T) {
	re := regexp.MustCompile(`(?i)\b(` + strings.Join(forbiddenGeneratedNames, "|") + `)\b`)

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "assets", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name == scannerFile {
			return nil
		}
		if ext := filepath.Ext(name); ext != ".go" && ext != ".md" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, m := range re.FindAllStringIndex(line, -1) {
				// encoding/json's own API is a standard-library call, not a name
				// this project invents for the wire format.
				if strings.HasSuffix(strings.ToLower(line[:m[0]]), "json.") {
					continue
				}
				t.Errorf("%s:%d: %q is not in the closed generated-object name set (CORELIB_PLAN §6.1.1); use encode/decode/serialize/deserialize\n\t%s",
					path, i+1, line[m[0]:m[1]], strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// The other half of §6.1.1: the docs must name the surface the Go backend does
// emit, so a reader can find it.
func TestDocsNameTheEmittedGeneratedSurface(t *testing.T) {
	for _, file := range []string{"README.md", "doc.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(src)
		for _, want := range []string{"Serialize", "Encode", "Decode"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: does not mention the generated %s surface", file, want)
			}
		}
	}
}

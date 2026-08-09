package sofab_test

// Documentation regression tests for the decode surface (CORELIB_PLAN §9.5,
// §9.6). §9.5 wants Usage to cover the port's decode entry points; §9.6 wants
// the memory section to say, per decode path, who owns the bytes handed to the
// caller and how long they must live. Both are per *entry point*, so both are
// checked against the entry points discovered in the source rather than against
// a list written down here: adding a third decode path (issue #87 added
// Decoder.AcceptStream, and the docs kept describing two) fails these tests
// until the docs grow the row that describes it.
//
// The last test then pins the fact the ownership row states, so the row is a
// tested contract and not a claim.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// decodeEntryPoints returns the public ways a caller can decode a message: the
// pull head (Decoder.Next) and every Accept* form, whether a method or a
// package-level function. They are read out of the package's own sources, so a
// new one cannot be added without the docs below noticing.
func decodeEntryPoints(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			name := fn.Name.Name
			switch {
			case fn.Recv == nil && strings.HasPrefix(name, "Accept"):
			case receiverTypeName(fn.Recv) == "Decoder" && (name == "Next" || strings.HasPrefix(name, "Accept")):
			default:
				continue
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	// Next, Accept, AcceptBytes, AcceptStream: a smaller set means the scan
	// stopped seeing the package, not that the package shrank.
	if len(names) < 4 {
		t.Fatalf("found only %v decode entry points; the source scan is broken", names)
	}
	return names
}

// receiverTypeName gives the (pointer-stripped) type a method hangs off, or ""
// for a plain function.
func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(src)
}

// §9.5: a decode entry point a reader cannot find in the prose does not exist
// for them. Both documentation surfaces must name it — the README and doc.go,
// which is the package overview the Docs badge publishes (§9.2/§9.4).
func TestDocsNameEveryDecodeEntryPoint(t *testing.T) {
	for _, file := range []string{"README.md", "doc.go"} {
		text := readDoc(t, file)
		for _, name := range decodeEntryPoints(t) {
			if !strings.Contains(text, name) {
				t.Errorf("%s: never mentions the %s decode entry point (CORELIB_PLAN §9.5)", file, name)
			}
		}
	}
}

// The "Why this design" row that enumerates the decode styles (§9.3) is where a
// reader learns which entry points exist at all, so it must name them all.
func TestDesignTableDecodeStylesRowNamesEveryEntryPoint(t *testing.T) {
	var row string
	for _, line := range strings.Split(readDoc(t, "README.md"), "\n") {
		if strings.HasPrefix(line, "|") && strings.Contains(line, "decode styles") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("README.md: no \"decode styles\" row in the design table")
	}
	for _, name := range decodeEntryPoints(t) {
		if !strings.Contains(row, name) {
			t.Errorf("README.md: the decode-styles row does not name %s:\n\t%s", name, strings.TrimSpace(row))
		}
	}
}

// §9.6: the ownership table states, per decode path, whether the values handed
// to the caller are copies or views. A path missing from it is a path whose
// ownership the docs never state.
func TestMemoryHandlingTableCoversEveryDecodePath(t *testing.T) {
	text := readDoc(t, "README.md")
	i := strings.Index(text, "## Memory handling")
	if i < 0 {
		t.Fatal("README.md: no \"## Memory handling\" section (CORELIB_PLAN §9.6)")
	}
	var rows []string
	inTable := false
	for _, line := range strings.Split(text[i:], "\n") {
		switch {
		case strings.HasPrefix(line, "| Path"):
			inTable = true
		case inTable && strings.HasPrefix(line, "|"):
			rows = append(rows, line)
		case inTable:
			inTable = false
		}
	}
	if len(rows) == 0 {
		t.Fatal("README.md: the memory-handling section has no decode ownership table (CORELIB_PLAN §9.6)")
	}
	for _, name := range decodeEntryPoints(t) {
		found := false
		for _, row := range rows {
			// The path is the row's first cell.
			if cells := strings.Split(row, "|"); len(cells) > 1 && strings.Contains(cells[1], name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("README.md: the decode ownership table has no %s row, so who owns its string/blob values is undocumented (CORELIB_PLAN §9.6)", name)
		}
	}
}

// keepV retains the string and blob it is handed, the way a visitor that binds
// them into a struct does.
type keepV struct {
	baseV
	str  string
	blob []byte
}

func (k *keepV) String(_ sofab.ID, s string) error { k.str = s; return nil }
func (k *keepV) Bytes(_ sofab.ID, b []byte) error  { k.blob = b; return nil }

// The ownership table's two observable claims, pinned: AcceptStream hands over
// fresh storage (its values survive the source bytes being overwritten), while
// AcceptBytes hands over views into the caller's slice (they do not). Scribbling
// over the input after the decode is exactly what a caller who reuses its
// receive buffer does.
func TestDecodePathOwnershipMatchesTheDocumentedTable(t *testing.T) {
	const str = "sofa"
	blob := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteString(1, str)
		e.WriteBytes(2, blob)
	})

	t.Run("AcceptStream copies", func(t *testing.T) {
		src := append([]byte(nil), msg...)
		var v keepV
		if err := sofab.NewDecoder(bytes.NewReader(src)).AcceptStream(&v); err != nil {
			t.Fatalf("AcceptStream: %v", err)
		}
		for i := range src {
			src[i] = 0xFF
		}
		if v.str != str {
			t.Errorf("AcceptStream string = %q after the source was overwritten, want %q (README: fresh copy)", v.str, str)
		}
		if !bytes.Equal(v.blob, blob) {
			t.Errorf("AcceptStream blob = %x after the source was overwritten, want %x (README: fresh copy)", v.blob, blob)
		}
	})

	t.Run("AcceptBytes aliases", func(t *testing.T) {
		src := append([]byte(nil), msg...)
		var v keepV
		if err := sofab.AcceptBytes(src, &v); err != nil {
			t.Fatalf("AcceptBytes: %v", err)
		}
		for i := range src {
			src[i] = 0xFF
		}
		if v.str != str {
			t.Errorf("AcceptBytes string = %q, want %q (README: fresh copy)", v.str, str)
		}
		if bytes.Equal(v.blob, blob) {
			t.Errorf("AcceptBytes blob survived the caller's slice being overwritten; README documents it as a view into that slice")
		}
	})
}

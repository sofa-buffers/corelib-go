package sofab_test

// What each decode path hands the caller: a fresh copy, or a view into memory
// the caller still owns. The difference decides whether a visitor may keep a
// value past the call, so it is pinned here rather than left as a claim.

import (
	"bytes"
	"os"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// readDoc reads a file the tests inspect as text (a Dockerfile, a script).
func readDoc(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(src)
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

// AcceptStream hands over fresh storage — its values survive the source bytes
// being overwritten — while AcceptBytes hands over views into the caller's
// slice, which do not. Scribbling over the input after the decode is exactly
// what a caller that reuses its receive buffer does.
func TestDecodePathOwnership(t *testing.T) {
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
			t.Errorf("AcceptStream string = %q after the source was overwritten, want %q", v.str, str)
		}
		if !bytes.Equal(v.blob, blob) {
			t.Errorf("AcceptStream blob = %x after the source was overwritten, want %x", v.blob, blob)
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
			t.Errorf("AcceptBytes string = %q, want %q", v.str, str)
		}
		if bytes.Equal(v.blob, blob) {
			t.Errorf("AcceptBytes blob survived the caller's slice being overwritten; it is a view into that slice")
		}
	})
}

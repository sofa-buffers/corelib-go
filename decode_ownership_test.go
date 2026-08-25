package sofab_test

// CORELIB_PLAN §6.7 / §6.0's chunk lifetime, as a checked property: whatever a
// callback receives is valid only until it returns, and a caller that keeps a
// value copies it. What that buys the caller is that the DECODED MESSAGE is
// unaffected by the input being reused, overwritten or freed afterwards — on the
// one-shot path exactly as on the streaming one (§6.7.1). That is what is pinned
// here, rather than left as a claim in the README.

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

// copyingV is the destination CORELIB_PLAN §6.7 describes: the codec "passes
// the value through the callback — the payload's total, this piece's offset, and
// the bytes themselves — and the caller copies it where it belongs". Both
// payloads arrive as windows into the fed chunk, so a destination that keeps one
// copies it; that is exactly what generated code emits, and PayloadAcc is the
// helper the corelib ships for it.
type copyingV struct {
	sofab.VisitorBase
	strAcc  sofab.PayloadAcc
	blobAcc sofab.PayloadAcc
	str     string
	blob    []byte
}

func (k *copyingV) String(_ sofab.ID, total, offset int, chunk []byte) error {
	if b, done := k.strAcc.Take(total, offset, chunk); done {
		k.str = string(b)
	}
	return nil
}

func (k *copyingV) Bytes(_ sofab.ID, total, offset int, chunk []byte) error {
	if b, done := k.blobAcc.Take(total, offset, chunk); done {
		k.blob = append([]byte(nil), b...)
	}
	return nil
}

// TestDecodedMessageSurvivesTheInputBeingScrubbed is §7.2 item 4's last two
// bullets: "Overwrite every chunk after `feed` returns" and "Overwrite the
// one-shot buffer too — run decode(buffer), scrub the whole buffer, and assert
// the decoded MESSAGE is unchanged." Scribbling over the input after the decode
// is exactly what a caller that reuses its receive buffer does.
//
// The obligation is on the decoded message, not on the callback argument: §6.7
// carries a value to the caller either by writing into storage the caller
// supplied, or by passing it through the callback for the caller to copy — and
// "the second route is not a view ... validity ends when the callback returns".
// Every entry point is held to it, because §6.7.1 gives the one-shot path no
// exemption: "that the caller supplied the whole buffer ... would make a view
// safe — but it would also make the port's decode behaviour depend on which
// entry point was used".
func TestDecodedMessageSurvivesTheInputBeingScrubbed(t *testing.T) {
	const str = "sofa"
	blob := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	msg := mustEncode(func(e *sofab.Encoder) {
		e.WriteString(1, str)
		e.WriteBytes(2, blob)
	})

	for _, s := range []struct {
		name   string
		decode func(src []byte, v sofab.Visitor) error
	}{
		{"AcceptBytes", func(src []byte, v sofab.Visitor) error { return acceptBytes(src, v) }},
		{"Feed", func(src []byte, v sofab.Visitor) error {
			return feedIn(src, 0, v)
		}},
		{"Feed/1-byte", func(src []byte, v sofab.Visitor) error {
			return feedIn(src, 1, v)
		}},
		{"FeedFrom/one-byte chunks", func(src []byte, v sofab.Visitor) error {
			return feedFrom(&chunkReader{b: src, n: 1}, 1, v)
		}},
	} {
		t.Run(s.name, func(t *testing.T) {
			src := append([]byte(nil), msg...)
			var v copyingV
			if err := s.decode(src, &v); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			for i := range src {
				src[i] = 0xFF
			}
			if v.str != str {
				t.Errorf("%s: string = %q after the input was scrubbed, want %q", s.name, v.str, str)
			}
			if !bytes.Equal(v.blob, blob) {
				t.Errorf("%s: blob = %x after the input was scrubbed, want %x", s.name, v.blob, blob)
			}
		})
	}
}

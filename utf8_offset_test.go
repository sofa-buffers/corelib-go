package sofab_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// A string field whose payload starts at a buffer offset AT OR BEYOND its own
// length must still be UTF-8 validated (CORELIB_PLAN §6.4).
//
// This is a known blind spot in the shared conformance set: every `invalid_utf8`
// vector puts the offending string near the front of the message, so the
// payload's start offset stays small relative to its length. A validator handed
// (buffer, start, length) that treats `length` as an EXCLUSIVE END index then
// walks an empty range whenever start >= length and accepts every malformed
// input — and the whole shared suite still passes. That is not hypothetical: it
// is exactly how a sibling backend's validator was broken while its conformance
// run stayed green.
//
// So the vectors below deliberately push the string LATE: a padding blob first,
// then a short invalid string, so its payload begins well past its own length.
// The property is asserted on the wire image itself (payloadStart >= payloadLen)
// before the decode, so a future edit that shortens the padding fails here rather
// than quietly reverting the test to the shape that hides the bug.
//
// Like the rest of the suite the assertions are written against
// utf8CheckCompiled (see utf8_build_on_test.go), so the footprint build — where
// §6.4 permits the validator to be compiled out — is held to ITS contract:
// the bytes survive verbatim, never replaced and never dropped.

// invalidUTF8Payloads are the malformed shapes §6.4 names, each short enough
// that any plausible padding puts its start offset past its length.
var invalidUTF8Payloads = map[string]string{
	"bad continuation":  "\xc3\x28",
	"lone continuation": "\x80\x80",
	"overlong NUL":      "\xc0\x80",
	"surrogate D800":    "\xed\xa0\x80",
	"truncated 3-byte":  "\xe2\x82",
	"above U+10FFFF":    "\xf5\x80\x80\x80",
}

// lateStringMsg builds [blob id 1 of padLen bytes][string id 2 = s] with strict
// UTF-8 off, so the malformed payload really reaches the wire, and returns the
// message together with the offset at which the string payload begins.
func lateStringMsg(t *testing.T, padLen int, s string) (msg []byte, payloadStart int) {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf, sofab.WithStrictUTF8(false))
	if err := e.WriteBytes(1, bytes.Repeat([]byte{'A'}, padLen)); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if err := e.WriteString(2, s); err != nil {
		t.Fatalf("WriteString with strict UTF-8 off: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	msg = buf.Bytes()
	i := bytes.LastIndex(msg, []byte(s))
	if i < 0 {
		t.Fatalf("the encoder did not write the payload verbatim: % x", msg)
	}
	return msg, i
}

// utf8Dest is the generated-code shape: it embeds StringCheck, so the decode's
// §6.4 policy is handed to it, and it rejects at the destination.
type utf8Dest struct {
	baseV
	sofab.StringCheck
	got string
}

func (d *utf8Dest) String(id sofab.ID, v string) error {
	if id != 2 {
		return nil
	}
	if !d.UTF8Valid([]byte(v)) {
		return sofab.ErrInvalidMsg
	}
	d.got = v
	return nil
}

func (d *utf8Dest) BeginSequence(sofab.ID) (sofab.Visitor, error) { return d, nil }

// checkLateInvalid asserts the verdict on one malformed payload placed late,
// across all three visitor entry points, under whichever half of the §6.4 gate
// this build compiled.
func checkLateInvalid(t *testing.T, bad string, padLen int) {
	t.Helper()
	msg, start := lateStringMsg(t, padLen, bad)
	if start < len(bad) {
		t.Fatalf("vector does not exercise the gap: payload starts at %d, length is %d", start, len(bad))
	}

	// (a) the zero-copy entry point: the payload is delivered as a slice of the
	// message buffer at a high offset, which is the exact shape the gap hides in.
	var dst utf8Dest
	err := sofab.AcceptBytes(msg, &dst)
	wantVisitor(t, "AcceptBytes", err, dst.got, bad)

	// (b) the slurping entry point.
	dst = utf8Dest{}
	err = sofab.NewDecoder(bytes.NewReader(msg)).Accept(&dst)
	wantVisitor(t, "Accept", err, dst.got, bad)

	// (c) the reader-driven entry point, fed one byte at a time so the field is
	// reassembled across chunk boundaries before it is judged.
	dst = utf8Dest{}
	err = sofab.NewDecoder(&chunkReader{b: append([]byte(nil), msg...), n: 1}).AcceptStream(&dst)
	wantVisitor(t, "AcceptStream(1-byte chunks)", err, dst.got, bad)
}

func wantVisitor(t *testing.T, what string, err error, got, bad string) {
	t.Helper()
	if utf8CheckCompiled {
		if !errors.Is(err, sofab.ErrInvalidMsg) {
			t.Errorf("%s = %v, want ErrInvalidMsg", what, err)
		}
		return
	}
	if err != nil || got != bad {
		t.Errorf("%s = %q, %v; the non-strict build must deliver the bytes verbatim", what, got, err)
	}
}

func TestInvalidUTF8AtAnOffsetPastItsOwnLength(t *testing.T) {
	for name, bad := range invalidUTF8Payloads {
		t.Run(name, func(t *testing.T) {
			// 200 bytes of padding: comfortably past the longest payload here.
			checkLateInvalid(t, bad, 200)
		})
	}
}

// The same offset, with the malformed sequence at the very END of a long
// payload: start offset past the length AND the offending byte far from the
// start, so a validator that walks the wrong range in either direction is
// caught.
func TestInvalidUTF8AtTheTailOfALateString(t *testing.T) {
	checkLateInvalid(t, strings.Repeat("ok-", 300)+"\xed\xa0\x80", 4096)
}

// The counter-case, so the tests above are shown to be measuring validation and
// not simply "a late string fails": the identical shape with a WELL-FORMED
// multi-byte payload — including multi-byte sequences split by the one-byte
// chunking — must decode clean and arrive intact in either build.
func TestValidUTF8AtTheSameLateOffsetIsAccepted(t *testing.T) {
	good := "ünïcödé — 漢字 🎉"
	msg, start := lateStringMsg(t, 200, good)
	if start < len(good) {
		t.Fatalf("vector does not exercise the gap: payload starts at %d, length is %d", start, len(good))
	}
	var dst utf8Dest
	if err := sofab.AcceptBytes(msg, &dst); err != nil {
		t.Fatalf("AcceptBytes = %v, want nil", err)
	}
	if dst.got != good {
		t.Errorf("AcceptBytes delivered %q, want %q", dst.got, good)
	}
	dst = utf8Dest{}
	if err := sofab.NewDecoder(&chunkReader{b: append([]byte(nil), msg...), n: 1}).AcceptStream(&dst); err != nil {
		t.Fatalf("AcceptStream(1-byte chunks) = %v, want nil", err)
	}
	if dst.got != good {
		t.Errorf("AcceptStream delivered %q, want %q", dst.got, good)
	}
}

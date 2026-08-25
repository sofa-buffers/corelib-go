package sofab_test

// Caller-supplied output buffer (CORELIB_PLAN §5.1) and the §7.2 item 4 encode
// tests that only become reachable with one: encoding into a buffer of exactly
// MinOutputBuffer, rejecting an undersized buffer where it is handed over,
// the taking-sink handover, the start offset, and the no-foreign-memory rule.

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// sampleMessage writes a message that mixes every writer surface and carries a
// string and a blob far longer than MinOutputBuffer, so the divisible-run split
// (§5.1) is exercised whatever buffer size the caller installs.
func sampleMessage(e *sofab.Encoder) {
	e.WriteUnsigned(1, 0xDEAD_BEEF_CAFE)
	e.WriteSigned(2, -1234567890)
	e.WriteBool(3, true)
	e.WriteString(4, strings.Repeat("sofa-buffers ", 40))
	e.WriteBytes(5, bytes.Repeat([]byte{0xA5, 0x5A, 0x00, 0xFF}, 50))
	e.WriteFloat32(6, 1.5)
	e.WriteFloat64(7, -2.25)
	sofab.WriteUnsignedArray(e, 8, []uint64{0, 1, 300, 1 << 62})
	sofab.WriteSignedArray(e, 9, []int32{-1, 0, 1, -70000})
	e.WriteFloat32Array(10, []float32{1, 2, 3})
	e.WriteFloat64Array(11, []float64{1, 2, 3})
	e.WriteSequenceBeginLazy(12)
	e.WriteUnsigned(1, 7)
	e.WriteString(2, "nested")
	e.WriteSequenceEnd()
}

// oneShot is the reference output: the same message through the io.Writer form,
// which buffers the whole thing and writes it in one go.
func oneShot(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := sofab.NewEncoder(&buf)
	sampleMessage(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("one-shot flush: %v", err)
	}
	return buf.Bytes()
}

// withinBuffer reports whether inner is a sub-slice of outer's backing array.
// Pointer identity per element rather than unsafe arithmetic: the buffers here
// are tens of bytes, and this is what proves a sink was handed the installed
// output buffer rather than foreign memory (§7.2 item 4).
func withinBuffer(outer, inner []byte) bool {
	if len(inner) == 0 {
		return true
	}
	o := outer[:cap(outer)]
	for k := range o {
		if &o[k] == &inner[0] {
			return k+len(inner) <= len(o)
		}
	}
	return false
}

// TestEncodeIntoMinOutputBuffer is §7.2 item 4's first encode bullet: a
// caller-supplied buffer of exactly MinOutputBuffer bytes with a flush sink,
// driven repeatedly, must produce the one-shot bytes.
func TestEncodeIntoMinOutputBuffer(t *testing.T) {
	want := oneShot(t)

	var got bytes.Buffer
	flushes := 0
	buf := make([]byte, sofab.MinOutputBuffer)
	e, err := sofab.NewEncoderSink(buf, 0, func(_ *sofab.Encoder, b []byte) error {
		flushes++
		got.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if flushes < 2 {
		t.Fatalf("sink driven %d time(s); a %d-byte buffer must flush repeatedly",
			flushes, sofab.MinOutputBuffer)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("streamed output (%d B) differs from one-shot (%d B)", got.Len(), len(want))
	}
}

// TestEncodeIntoBufferSizes walks a range of caller-supplied buffer sizes: every
// one at or above the minimum must work and must be byte-identical (§5.1).
func TestEncodeIntoBufferSizes(t *testing.T) {
	want := oneShot(t)
	sizes := []int{4096, 8192, len(want), len(want) + 1}
	for s := sofab.MinOutputBuffer; s <= 128; s++ {
		sizes = append(sizes, s)
	}
	for _, size := range sizes {
		var got bytes.Buffer
		buf := make([]byte, size)
		e, err := sofab.NewEncoderSink(buf, 0, func(_ *sofab.Encoder, b []byte) error {
			got.Write(b)
			return nil
		})
		if err != nil {
			t.Fatalf("size %d: NewEncoderSink: %v", size, err)
		}
		sampleMessage(e)
		if err := e.Flush(); err != nil {
			t.Fatalf("size %d: flush: %v", size, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("size %d: output differs from one-shot", size)
		}
	}
}

// TestStartOffset is the §5.1 framing-header case: the encoder leaves the first
// offset bytes of the caller's buffer alone and starts writing after them, so a
// framing header can be filled in without a second buffer or a copy.
func TestStartOffset(t *testing.T) {
	want := oneShot(t)

	// No sink: the offset is never consumed by a flush, so the reserved room
	// stays untouched for the whole message.
	const offset = 8
	buf := make([]byte, offset+len(want)+64)
	for i := range buf {
		buf[i] = 0x7E // framing-header fill
	}
	e, err := sofab.NewEncoderBuffer(buf, offset)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := e.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("offset output (%d B) differs from one-shot (%d B)", len(got), len(want))
	}
	if !withinBuffer(buf[offset:], got) {
		t.Fatalf("message did not start at the installation offset")
	}
	for i := 0; i < offset; i++ {
		if buf[i] != 0x7E {
			t.Fatalf("encoder wrote into the reserved header room at [%d]", i)
		}
	}
	for i := offset + len(got); i < len(buf); i++ {
		if buf[i] != 0x7E {
			t.Fatalf("encoder wrote past the message at [%d]", i)
		}
	}

	// With a sink, the offset belongs to the installation and is consumed by it:
	// the first flushed unit starts after the reserved room, later ones at 0
	// (§5.1). Re-arming it per unit is TestTakingSinkWithFreshOffset.
	small := make([]byte, offset+sofab.MinOutputBuffer)
	var streamed bytes.Buffer
	first := true
	se, err := sofab.NewEncoderSink(small, offset, func(_ *sofab.Encoder, b []byte) error {
		if first {
			if !withinBuffer(small[offset:], b) {
				t.Errorf("first flushed unit did not start at the installation offset")
			}
			first = false
		}
		streamed.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	sampleMessage(se)
	if err := se.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !bytes.Equal(streamed.Bytes(), want) {
		t.Fatalf("offset+sink output differs from one-shot")
	}
}

// TestBufferBelowMinimumRejected is §7.2 item 4's second encode bullet: a buffer
// installed WITH a sink one byte short of the minimum is rejected where it is
// handed over, not partway through a message.
func TestBufferBelowMinimumRejected(t *testing.T) {
	sink := func(_ *sofab.Encoder, b []byte) error { return nil }

	small := make([]byte, sofab.MinOutputBuffer-1)
	if _, err := sofab.NewEncoderSink(small, 0, sink); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("NewEncoderSink(%d B) = %v, want ErrArgument", len(small), err)
	}
	// buflen - offset is what the minimum binds, not buflen.
	big := make([]byte, 64)
	if _, err := sofab.NewEncoderSink(big, len(big)-(sofab.MinOutputBuffer-1), sink); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("NewEncoderSink(offset leaving %d B) = %v, want ErrArgument", sofab.MinOutputBuffer-1, err)
	}
	// Exactly the minimum is accepted.
	if _, err := sofab.NewEncoderSink(big, len(big)-sofab.MinOutputBuffer, sink); err != nil {
		t.Fatalf("NewEncoderSink(offset leaving exactly the minimum) = %v, want nil", err)
	}
	// And the same rejection at a mid-stream buffer-set.
	e, err := sofab.NewEncoderSink(big, 0, sink)
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	if err := e.SetBuffer(small, 0); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("SetBuffer(%d B) = %v, want ErrArgument", len(small), err)
	}
	// An out-of-range offset is rejected the same way.
	if err := e.SetBuffer(big, len(big)+1); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("SetBuffer(offset past the end) = %v, want ErrArgument", err)
	}
	if err := e.SetBuffer(big, -1); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("SetBuffer(negative offset) = %v, want ErrArgument", err)
	}
}

// TestUndersizedBufferWithoutSink is the converse the same bullet demands: the
// minimum is a streaming constant and must not become a floor on the one-shot
// path. A two-byte message encodes into a two-byte buffer.
func TestUndersizedBufferWithoutSink(t *testing.T) {
	// The very buffer NewEncoderSink turned away, accepted here, holding a
	// message that fits.
	under := make([]byte, sofab.MinOutputBuffer-1)
	ue, err := sofab.NewEncoderBuffer(under, 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer(%d B): %v", len(under), err)
	}
	ue.WriteUnsigned(1, 0xFFFF_FFFF_FFFF_FFFF) // 1 B header + 10 B value
	ue.WriteSigned(2, -3)
	if err := ue.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	var ref bytes.Buffer
	re := sofab.NewEncoder(&ref)
	re.WriteUnsigned(1, 0xFFFF_FFFF_FFFF_FFFF)
	re.WriteSigned(2, -3)
	if err := re.Flush(); err != nil {
		t.Fatalf("reference flush: %v", err)
	}
	if got := ue.Bytes(); !bytes.Equal(got, ref.Bytes()) {
		t.Fatalf("undersized-buffer output % x, want % x", got, ref.Bytes())
	}

	buf := make([]byte, 2)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer(2 B): %v", err)
	}
	if err := e.WriteUnsigned(0, 127); err != nil {
		t.Fatalf("WriteUnsigned: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := e.Bytes(); !bytes.Equal(got, []byte{0x00, 0x7F}) {
		t.Fatalf("Bytes() = % x, want 00 7f", got)
	}
	// Zero-length buffers and any offset within the buffer are accepted too.
	if _, err := sofab.NewEncoderBuffer(nil, 0); err != nil {
		t.Fatalf("NewEncoderBuffer(nil): %v", err)
	}
	if _, err := sofab.NewEncoderBuffer(buf, len(buf)); err != nil {
		t.Fatalf("NewEncoderBuffer(offset == len): %v", err)
	}
	if _, err := sofab.NewEncoderBuffer(buf, len(buf)+1); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("NewEncoderBuffer(offset past the end) = %v, want ErrArgument", err)
	}
}

// TestSinkLessBufferIsExact: with no sink the buffer is measured in bytes
// actually written, not in bytes a writer reserved. A buffer of exactly the
// message size holds it; one byte less reports ErrBufferFull (§5.1).
func TestSinkLessBufferIsExact(t *testing.T) {
	want := oneShot(t)
	for _, offset := range []int{0, 1, 7} {
		exact := make([]byte, offset+len(want))
		e, err := sofab.NewEncoderBuffer(exact, offset)
		if err != nil {
			t.Fatalf("offset %d: NewEncoderBuffer: %v", offset, err)
		}
		sampleMessage(e)
		if err := e.Flush(); err != nil {
			t.Fatalf("offset %d: exact-size buffer: %v", offset, err)
		}
		if got := e.Bytes(); !bytes.Equal(got, want) {
			t.Fatalf("offset %d: exact-size output (%d B) differs from one-shot (%d B)",
				offset, len(got), len(want))
		}

		short := make([]byte, offset+len(want)-1)
		e, err = sofab.NewEncoderBuffer(short, offset)
		if err != nil {
			t.Fatalf("offset %d: NewEncoderBuffer: %v", offset, err)
		}
		sampleMessage(e)
		if err := e.Flush(); !errors.Is(err, sofab.ErrBufferFull) {
			t.Fatalf("offset %d: one byte short = %v, want ErrBufferFull", offset, err)
		}
	}
}

// TestBufferFullWithoutSink: a buffer that fills with no sink reports
// buffer-full rather than overflowing or reporting partial output as complete
// (§5.1).
func TestBufferFullWithoutSink(t *testing.T) {
	buf := make([]byte, 8)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); !errors.Is(err, sofab.ErrBufferFull) {
		t.Fatalf("flush = %v, want ErrBufferFull", err)
	}
	if err := e.Err(); !errors.Is(err, sofab.ErrBufferFull) {
		t.Fatalf("Err() = %v, want ErrBufferFull", err)
	}
	// The error is sticky: no later write silently succeeds.
	if err := e.WriteUnsigned(1, 1); !errors.Is(err, sofab.ErrBufferFull) {
		t.Fatalf("post-error write = %v, want ErrBufferFull", err)
	}
	// A blob whose payload alone overruns the buffer is reported too.
	e2, err := sofab.NewEncoderBuffer(make([]byte, 16), 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	if err := e2.WriteBytes(1, bytes.Repeat([]byte{1}, 64)); !errors.Is(err, sofab.ErrBufferFull) {
		t.Fatalf("WriteBytes into a 16 B buffer = %v, want ErrBufferFull", err)
	}
}

// TestTakingSink is §7.2 item 4's third encode bullet: a sink that takes the
// buffer it was handed, scrubs it and installs a replacement before returning.
// An encoder that kept writing into the buffer it gave away would read back the
// fill pattern.
func TestTakingSink(t *testing.T) {
	want := oneShot(t)

	var got bytes.Buffer
	installs := 0
	e, err := sofab.NewEncoderSink(make([]byte, sofab.MinOutputBuffer), 0,
		func(enc *sofab.Encoder, b []byte) error {
			got.Write(b) // the "transport" takes a copy of its own
			for i := range b {
				b[i] = 0xEE // scrub: this storage is ours now
			}
			installs++
			return enc.SetBuffer(make([]byte, sofab.MinOutputBuffer), 0)
		})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if installs < 2 {
		t.Fatalf("sink installed %d replacement(s); expected repeated handover", installs)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("taking-sink output differs from one-shot")
	}
}

// TestTakingSinkWithFreshOffset covers the other half of §5.1's installation
// rule: passing the SAME buffer to SetBuffer re-arms the start offset, which is
// how a sink gets framing-header room in every flushed unit.
func TestTakingSinkWithFreshOffset(t *testing.T) {
	want := oneShot(t)

	const offset = 4
	buf := make([]byte, offset+sofab.MinOutputBuffer)
	var got bytes.Buffer
	units := 0
	e, err := sofab.NewEncoderSink(buf, offset, func(enc *sofab.Encoder, b []byte) error {
		units++
		if !withinBuffer(buf[offset:], b) {
			t.Errorf("flushed unit %d did not start at the re-armed offset", units)
		}
		got.Write(b)
		return enc.SetBuffer(buf, offset) // new installation, header room again
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if units < 2 {
		t.Fatalf("only %d flushed unit(s)", units)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("re-armed-offset output differs from one-shot")
	}
}

// TestCopyingSink is the other half of the returning-callback contract: a sink
// that returns without installing anything has copied, and the encoder resumes
// in the same buffer at offset 0.
func TestCopyingSink(t *testing.T) {
	want := oneShot(t)

	buf := make([]byte, sofab.MinOutputBuffer)
	var got bytes.Buffer
	e, err := sofab.NewEncoderSink(buf, 0, func(_ *sofab.Encoder, b []byte) error {
		if !withinBuffer(buf, b) {
			t.Errorf("copying sink was handed memory outside the installed buffer")
		}
		got.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("copying-sink output differs from one-shot")
	}
}

// TestNoForeignMemory is §7.2 item 4's fourth encode bullet: a sink NEVER
// receives memory outside the installed buffer, however large the payload.
// Pass-through is forbidden (§5.1.6) — "there is no permission flag to set and
// no exemption to claim" — so this holds unconditionally, on every flush of
// every message.
func TestNoForeignMemory(t *testing.T) {
	blob := bytes.Repeat([]byte{0x31, 0x41, 0x59, 0x26}, 4096) // 16 KiB
	str := strings.Repeat("pass-through?", 1000)

	buf := make([]byte, 64)
	var got bytes.Buffer
	e, err := sofab.NewEncoderSink(buf, 0, func(_ *sofab.Encoder, b []byte) error {
		if !withinBuffer(buf, b) {
			t.Fatalf("sink was handed memory outside the installed buffer (§5.1.6)")
		}
		got.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	e.WriteBytes(1, blob)
	e.WriteString(2, str)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var want bytes.Buffer
	ref := sofab.NewEncoder(&want)
	ref.WriteBytes(1, blob)
	ref.WriteString(2, str)
	if err := ref.Flush(); err != nil {
		t.Fatalf("reference flush: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("copied-payload output differs from one-shot")
	}
}

// TestIOWriterFormRejectsBufferSet: the io.Writer form has no caller-supplied
// buffer to replace, so installing one is ErrArgument.
func TestIOWriterFormRejectsBufferSet(t *testing.T) {
	we := sofab.NewEncoder(&bytes.Buffer{})
	if err := we.SetBuffer(make([]byte, 64), 0); !errors.Is(err, sofab.ErrArgument) {
		t.Fatalf("SetBuffer on the io.Writer form = %v, want ErrArgument", err)
	}
}

// foreignWatcher is the io.Writer form's equivalent of withinBuffer: it
// records whether the destination was ever handed the caller's own payload
// instead of bytes copied through the encoder's window. It implements
// io.StringWriter, which is the surface a passed-through string would arrive on.
//
// A blob is caught by pointer identity. A string that reaches Write instead
// (after a []byte conversion) is caught by carrying the whole payload in one
// call, which the copy path — bounded by the window — never does.
type foreignWatcher struct {
	out         bytes.Buffer
	blob        []byte
	str         string
	foreignBlob int
	foreignStr  int
}

func (w *foreignWatcher) Write(p []byte) (int, error) {
	if len(p) != 0 && len(w.blob) != 0 && withinBuffer(w.blob, p) {
		w.foreignBlob++
	}
	if w.str != "" && len(p) == len(w.str) && string(p) == w.str {
		w.foreignStr++
	}
	return w.out.Write(p)
}

func (w *foreignWatcher) WriteString(s string) (int, error) {
	if w.str != "" && s == w.str {
		w.foreignStr++
	}
	return w.out.WriteString(s)
}

// foreignTestMessage writes the message the io.Writer-form no-foreign-memory
// test compares, and foreignTestReference is its expected bytes, produced
// through a sink-less caller-supplied buffer so the reference never travels the
// path under test.
func foreignTestMessage(e *sofab.Encoder, blob []byte, str string) {
	e.WriteUnsigned(0, 1)
	e.WriteBytes(1, blob)
	e.WriteString(2, str)
}

func foreignTestReference(t *testing.T, blob []byte, str string) []byte {
	t.Helper()
	e, err := sofab.NewEncoderBuffer(make([]byte, len(blob)+len(str)+64), 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	foreignTestMessage(e, blob, str)
	if err := e.Flush(); err != nil {
		t.Fatalf("reference flush: %v", err)
	}
	return e.Bytes()
}

// TestIOWriterNoForeignMemory is §7.2 item 4's "no foreign memory, ever" bullet
// for the io.Writer form: however large the payload, the destination only ever
// sees bytes copied through the encoder's own scratch window (§5.1.6).
func TestIOWriterNoForeignMemory(t *testing.T) {
	blob := bytes.Repeat([]byte{0x31, 0x41, 0x59, 0x26}, 4096) // 16 KiB
	str := strings.Repeat("pass-through?", 2000)               // 26 KiB

	w := &foreignWatcher{blob: blob, str: str}
	e := sofab.NewEncoder(w)
	foreignTestMessage(e, blob, str)
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if w.foreignBlob != 0 || w.foreignStr != 0 {
		t.Fatalf("io.Writer was handed the caller's own payload (blob %d, string %d); §5.1.6 forbids it",
			w.foreignBlob, w.foreignStr)
	}
	if !bytes.Equal(w.out.Bytes(), foreignTestReference(t, blob, str)) {
		t.Fatalf("copied-payload output differs from the reference")
	}
}

// TestSinkErrorIsSticky: a sink failure is the encoder's sticky error, and no
// partial output is reported as complete (§5.1).
func TestSinkErrorIsSticky(t *testing.T) {
	boom := errors.New("sink failed")
	e, err := sofab.NewEncoderSink(make([]byte, sofab.MinOutputBuffer), 0,
		func(_ *sofab.Encoder, b []byte) error { return boom })
	if err != nil {
		t.Fatalf("NewEncoderSink: %v", err)
	}
	sampleMessage(e)
	if err := e.Flush(); !errors.Is(err, boom) {
		t.Fatalf("flush = %v, want the sink error", err)
	}
}

// TestBufferSetOutsideFlush: a no-sink caller may drain the buffer itself and
// install the next one, which is the manual form of the same handover.
func TestBufferSetOutsideFlush(t *testing.T) {
	var got bytes.Buffer
	buf := make([]byte, sofab.MinOutputBuffer)
	e, err := sofab.NewEncoderBuffer(buf, 0)
	if err != nil {
		t.Fatalf("NewEncoderBuffer: %v", err)
	}
	writes := []func(){
		func() { e.WriteUnsigned(1, 0xDEAD_BEEF_CAFE) },
		func() { e.WriteSigned(2, -1234567890) },
		func() { e.WriteFloat64(3, -2.25) },
	}
	var ref bytes.Buffer
	re := sofab.NewEncoder(&ref)
	re.WriteUnsigned(1, 0xDEAD_BEEF_CAFE)
	re.WriteSigned(2, -1234567890)
	re.WriteFloat64(3, -2.25)
	re.Flush()

	for _, w := range writes {
		w()
		if err := e.Err(); err != nil {
			t.Fatalf("write: %v", err)
		}
		got.Write(e.Bytes())
		if err := e.SetBuffer(buf, 0); err != nil {
			t.Fatalf("SetBuffer: %v", err)
		}
	}
	if !bytes.Equal(got.Bytes(), ref.Bytes()) {
		t.Fatalf("manual-handover output differs from one-shot")
	}
}

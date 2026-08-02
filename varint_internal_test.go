package sofab

// The varint writer is unrolled in three places — putUvarint (varint.go, the
// single-value entry point behind headers, counts and scalars) and the two
// specialized array runs putUvarintRun / putZigzagRun (encoder.go) — because
// calling out of the bulk loops cost about a quarter of the array-encode
// profile. Three copies of bit-manipulation code is a standing hazard, so these
// tests pin them to each other and to the decoder: a change made to one and not
// the others fails here rather than silently corrupting the wire.
//
// They live in package sofab (not sofab_test) because putUvarint and the run
// helpers are unexported.

import (
	"bytes"
	"math"
	"testing"
)

// varintProbes are the values worth checking: every 7-bit boundary and the byte
// either side of it (where the encoded length changes), the extremes, and a
// spread of high-entropy values that exercise the full ten-byte path.
func varintProbes() []uint64 {
	v := []uint64{0, 1, 2, 127, 128, 129, math.MaxUint64, math.MaxUint64 - 1, math.MaxInt64, 1 << 63}
	for shift := 0; shift < 64; shift += 7 {
		b := uint64(1) << shift
		v = append(v, b-1, b, b+1)
	}
	for i := uint64(0); i < 512; i++ {
		v = append(v, i*0x9E3779B97F4A7C15)
	}
	return v
}

// refUvarint is an independent, deliberately naive base-128 encoder. It is the
// spec statement (CORELIB_PLAN §4.1) the three unrolled writers are checked
// against, so a shared mistake in the unrolled form cannot hide behind them all
// agreeing with each other.
func refUvarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func TestPutUvarintMatchesReference(t *testing.T) {
	buf := make([]byte, maxVarintLen)
	for _, v := range varintProbes() {
		n := putUvarint(buf, 0, v)
		if got, want := buf[:n], refUvarint(v); !bytes.Equal(got, want) {
			t.Fatalf("putUvarint(%d) = % x, want % x", v, got, want)
		}
	}
}

// TestVarintWritersAgree is the differential check across the three copies: the
// array runs must emit exactly what putUvarint emits for the same values.
func TestVarintWritersAgree(t *testing.T) {
	probes := varintProbes()

	// Unsigned: putUvarintRun vs putUvarint, element for element.
	var want bytes.Buffer
	buf := make([]byte, maxVarintLen)
	for _, v := range probes {
		want.Write(buf[:putUvarint(buf, 0, v)])
	}
	var got bytes.Buffer
	e := NewEncoder(&got)
	if err := putUvarintRun(e, probes); err != nil {
		t.Fatalf("putUvarintRun: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	assertSameBytes(t, "putUvarintRun", got.Bytes(), want.Bytes())

	// Signed: putZigzagRun must equal putUvarint of the zigzag mapping.
	signed := make([]int64, 0, len(probes)*2)
	for _, v := range probes {
		signed = append(signed, int64(v), -int64(v))
	}
	want.Reset()
	for _, v := range signed {
		want.Write(buf[:putUvarint(buf, 0, zigzagEncode(v))])
	}
	got.Reset()
	e = NewEncoder(&got)
	if err := putZigzagRun(e, signed); err != nil {
		t.Fatalf("putZigzagRun: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	assertSameBytes(t, "putZigzagRun", got.Bytes(), want.Bytes())
}

// TestVarintDecodersAgree pins the two decode paths — uvarintFast (bulk, no
// end-of-buffer test) and uvarintTail (near the buffer end, bounds checked) — to
// each other and to what the writers produced. The split between them depends on
// how close the varint sits to the end of the buffer, so each probe is decoded
// both ways.
func TestVarintDecodersAgree(t *testing.T) {
	for _, v := range varintProbes() {
		enc := refUvarint(v)

		// Padded: at least maxVarintLen bytes available, so uvarintFast applies.
		padded := append(append([]byte(nil), enc...), make([]byte, maxVarintLen)...)
		gotFast, npFast, ok := uvarintFast(padded, 0)
		if !ok {
			t.Fatalf("uvarintFast(%d) reported overflow", v)
		}
		if gotFast != v || npFast != len(enc) {
			t.Fatalf("uvarintFast(%d) = (%d, %d), want (%d, %d)", v, gotFast, npFast, v, len(enc))
		}

		// Exact: nothing after the varint, so the tail path applies.
		gotTail, npTail, st := uvarintTail(enc, 0)
		if st != varintOK {
			t.Fatalf("uvarintTail(%d) status %v", v, st)
		}
		if gotTail != v || npTail != len(enc) {
			t.Fatalf("uvarintTail(%d) = (%d, %d), want (%d, %d)", v, gotTail, npTail, v, len(enc))
		}
	}
}

// TestBulkDecoderMatchesSingle pins decodeUvarintRun — the bulk path both array
// element types go through — to uvarintFast/uvarintTail on the same bytes. The
// chunking and the stop-at-tail rule are where a bulk decoder can silently drift
// from the single-value one, so the run is driven at several chunk sizes and
// with the values packed right up against the end of the buffer.
func TestBulkDecoderMatchesSingle(t *testing.T) {
	probes := varintProbes()

	var stream []byte
	for _, v := range probes {
		stream = append(stream, refUvarint(v)...)
	}

	for _, chunk := range []int{1, 2, 7, varintChunk, len(probes)} {
		got := make([]uint64, 0, len(probes))
		p := 0
		for len(got) < len(probes) {
			want := min(chunk, len(probes)-len(got))
			dst := make([]uint64, want)
			n, np, st := decodeUvarintRun(stream, p, dst)
			if st != varintOK {
				t.Fatalf("chunk %d: decodeUvarintRun status %v at %d", chunk, st, p)
			}
			got = append(got, dst[:n]...)
			p = np
			if n < want {
				// Tail region: finish with the bounds-checked single decoder,
				// exactly as fillUnsigned/fillSigned do.
				for len(got) < len(probes) {
					v, tp, tst := uvarintTail(stream, p)
					if tst != varintOK {
						t.Fatalf("chunk %d: uvarintTail status %v at %d", chunk, tst, p)
					}
					got = append(got, v)
					p = tp
				}
			}
		}
		if p != len(stream) {
			t.Fatalf("chunk %d: consumed %d of %d bytes", chunk, p, len(stream))
		}
		for i, v := range probes {
			if got[i] != v {
				t.Fatalf("chunk %d: element %d = %d, want %d", chunk, i, got[i], v)
			}
		}
	}
}

// TestBulkDecoderRejectsOverlong pins the >64-bit rule in the bulk path too: it
// must report overflow rather than truncating the value or the run.
func TestBulkDecoderRejectsOverlong(t *testing.T) {
	stream := append(refUvarint(7), 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02)
	stream = append(stream, make([]byte, maxVarintLen)...) // keep the run out of the tail region
	dst := make([]uint64, 2)
	got, _, st := decodeUvarintRun(stream, 0, dst)
	if st != varintOverflow {
		t.Fatalf("decodeUvarintRun status %v, want overflow", st)
	}
	if got != 1 || dst[0] != 7 {
		t.Fatalf("decoded %d elements (first = %d), want 1 element = 7", got, dst[0])
	}
}

// TestFillRoundTripsEveryProbe drives the two cursor fill paths over a real
// encoded array, which is what the visitor decode actually calls. It closes the
// loop: the writers in encoder.go produce the bytes, the bulk decoder reads them
// back, and the values must survive unchanged.
func TestFillRoundTripsEveryProbe(t *testing.T) {
	probes := varintProbes()
	signed := make([]int64, len(probes))
	for i, v := range probes {
		if i%2 == 0 {
			signed[i] = int64(v)
		} else {
			signed[i] = -int64(v >> 1)
		}
	}

	var buf bytes.Buffer
	e := NewEncoder(&buf)
	if err := WriteUnsignedArray(e, 1, probes); err != nil {
		t.Fatalf("WriteUnsignedArray: %v", err)
	}
	if err := WriteSignedArray(e, 2, signed); err != nil {
		t.Fatalf("WriteSignedArray: %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	c := cursor{buf: buf.Bytes()}
	// Unsigned array: header, count, then the elements.
	if _, err := c.uvarint(false); err != nil {
		t.Fatalf("header: %v", err)
	}
	n, err := c.uvarint(false)
	if err != nil || int(n) != len(probes) {
		t.Fatalf("count = %d (%v), want %d", n, err, len(probes))
	}
	gotU := make([]uint64, n)
	if err := c.fillUnsigned(gotU); err != nil {
		t.Fatalf("fillUnsigned: %v", err)
	}
	for i, v := range probes {
		if gotU[i] != v {
			t.Fatalf("unsigned element %d = %d, want %d", i, gotU[i], v)
		}
	}

	if _, err := c.uvarint(false); err != nil {
		t.Fatalf("signed header: %v", err)
	}
	if n, err = c.uvarint(false); err != nil || int(n) != len(signed) {
		t.Fatalf("signed count = %d (%v), want %d", n, err, len(signed))
	}
	gotS := make([]int64, n)
	if err := c.fillSigned(gotS); err != nil {
		t.Fatalf("fillSigned: %v", err)
	}
	for i, v := range signed {
		if gotS[i] != v {
			t.Fatalf("signed element %d = %d, want %d", i, gotS[i], v)
		}
	}
	if c.pos != len(c.buf) {
		t.Fatalf("consumed %d of %d bytes", c.pos, len(c.buf))
	}
}

// TestVarintDecodersRejectOverlong pins the >64-bit rule (§4.1) in both decode
// paths: an eleventh byte, and a tenth byte whose payload spills past bit 63,
// are malformed regardless of what follows.
func TestVarintDecodersRejectOverlong(t *testing.T) {
	overlong := [][]byte{
		// Ten continuation bytes demanding an eleventh.
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01},
		// Tenth byte payload 2: one bit above the 64-bit bound.
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02},
		// Tenth byte with a continuation flag set.
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x81},
	}
	for _, enc := range overlong {
		padded := append(append([]byte(nil), enc...), make([]byte, maxVarintLen)...)
		if _, _, ok := uvarintFast(padded, 0); ok {
			t.Fatalf("uvarintFast accepted overlong % x", enc)
		}
		if _, _, st := uvarintTail(enc, 0); st != varintOverflow {
			t.Fatalf("uvarintTail(% x) status %v, want overflow", enc, st)
		}
	}

	// The tenth byte carrying payload 1 is the largest legal varint, and a tenth
	// byte of 0 is non-minimal but in range: §4.1 requires both to decode.
	for _, tc := range []struct {
		enc  []byte
		want uint64
	}{
		{[]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, 1 << 63},
		{[]byte{0x81, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00}, 1},
	} {
		padded := append(append([]byte(nil), tc.enc...), make([]byte, maxVarintLen)...)
		v, _, ok := uvarintFast(padded, 0)
		if !ok || v != tc.want {
			t.Fatalf("uvarintFast(% x) = (%d, %v), want (%d, true)", tc.enc, v, ok, tc.want)
		}
		v, _, st := uvarintTail(tc.enc, 0)
		if st != varintOK || v != tc.want {
			t.Fatalf("uvarintTail(% x) = (%d, %v), want (%d, ok)", tc.enc, v, st, tc.want)
		}
	}
}

// assertSameBytes reports the first differing offset, which is what identifies
// the unrolled block that drifted.
func assertSameBytes(t *testing.T, who string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			lo := max(0, i-4)
			t.Fatalf("%s disagrees with putUvarint at byte %d (len got=%d want=%d)\n got % x\nwant % x",
				who, i, len(got), len(want), got[lo:min(len(got), i+5)], want[lo:min(len(want), i+5)])
		}
	}
	t.Fatalf("%s disagrees with putUvarint: got %d B, want %d B", who, len(got), len(want))
}

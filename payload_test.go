package sofab_test

// Unit tests for the chunk-reassembly accumulator (payload.go). CORELIB_PLAN
// §7 makes them mandatory, and here they are the only possible coverage: the
// accumulator never touches the wire, so no shared vector can tell a correct
// one from one that splices a payload back together in the wrong order.
//
// The central test is the one §6 asks for in words — "a chunk boundary may fall
// anywhere" — as a loop: the same payload split at EVERY offset must reassemble
// to the same bytes.

import (
	"bytes"
	"math"
	"runtime"
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// payloadOf builds n bytes no two of which are equal modulo 251, so a chunk
// placed at the wrong offset cannot pass by accident.
func payloadOf(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + 1)
	}
	return p
}

// The property a chunked decoder stands on: where the boundary falls must not
// change the result. Split at 0 and at n included — the ends are where the
// known gaps in the family's invalid_utf8 vectors sit (an offset that never
// reaches total, and one that starts at it).
func TestPayloadAccSplitAtEveryOffset(t *testing.T) {
	want := payloadOf(37)

	for k := 0; k <= len(want); k++ {
		var acc sofab.PayloadAcc

		p, done := acc.Take(len(want), 0, want[:k])
		if k == len(want) {
			if !done || !bytes.Equal(p, want) {
				t.Fatalf("split %d: first chunk = (%v, %v), want the whole payload", k, p, done)
			}
			continue
		}
		if done {
			t.Fatalf("split %d: reported complete after %d of %d bytes", k, k, len(want))
		}

		p, done = acc.Take(len(want), k, want[k:])
		if !done {
			t.Fatalf("split %d: still incomplete after the last chunk", k)
		}
		if !bytes.Equal(p, want) {
			t.Fatalf("split %d: payload = %v, want %v", k, p, want)
		}
	}
}

// The extreme of the same property: one byte at a time.
func TestPayloadAccByteAtATime(t *testing.T) {
	want := payloadOf(19)
	var acc sofab.PayloadAcc

	for i := 0; i < len(want); i++ {
		p, done := acc.Take(len(want), i, want[i:i+1])
		if i < len(want)-1 {
			if done {
				t.Fatalf("byte %d: reported complete early", i)
			}
			continue
		}
		if !done || !bytes.Equal(p, want) {
			t.Fatalf("last byte: (%v, %v), want the whole payload", p, done)
		}
	}
}

// A payload arriving whole in the first chunk is handed back as-is: no copy, no
// allocation. It therefore aliases the caller's buffer, exactly as the slices
// AcceptBytes hands a visitor do.
func TestPayloadAccSingleChunkIsPassedThrough(t *testing.T) {
	want := payloadOf(8)
	var acc sofab.PayloadAcc

	allocs := testing.AllocsPerRun(100, func() {
		if p, done := acc.Take(len(want), 0, want); !done || &p[0] != &want[0] {
			t.Fatalf("one-chunk payload was copied or reported incomplete (done=%v)", done)
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations for a one-chunk payload, want 0", allocs)
	}
}

// A chunk may carry more than this payload — the bytes of whatever follows it.
// Only the announced length belongs to the caller.
func TestPayloadAccTrimsAnOverLongChunk(t *testing.T) {
	var acc sofab.PayloadAcc
	p, done := acc.Take(3, 0, []byte{1, 2, 3, 4, 5})
	if !done || !bytes.Equal(p, []byte{1, 2, 3}) {
		t.Fatalf("(%v, %v), want the first 3 bytes", p, done)
	}
}

// A zero-length payload is complete the moment it is announced: there is no
// chunk that could finish it later.
func TestPayloadAccZeroLengthPayload(t *testing.T) {
	var acc sofab.PayloadAcc
	if p, done := acc.Take(0, 0, nil); !done || len(p) != 0 {
		t.Fatalf("(%v, %v), want an empty complete payload", p, done)
	}
}

// Chunks must be contiguous. A gap, an overlap, or a chunk announcing another
// length is refused rather than spliced in at the wrong place — the caller gets
// "incomplete", and the next offset-0 chunk starts a clean payload.
func TestPayloadAccRefusesNonContiguousChunks(t *testing.T) {
	want := payloadOf(6)

	for _, tc := range []struct {
		name          string
		total, offset int
		chunk         []byte
	}{
		{"gap", 6, 4, want[4:]},
		{"overlap", 6, 1, want[1:]},
		{"different total", 7, 3, want[3:]},
		{"negative offset", 6, -1, want[3:]},
		{"negative total", -1, 0, want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var acc sofab.PayloadAcc
			if _, done := acc.Take(6, 0, want[:3]); done {
				t.Fatal("first chunk reported complete")
			}
			if p, done := acc.Take(tc.total, tc.offset, tc.chunk); done {
				t.Fatalf("(%v, true), want the chunk refused", p)
			}

			// The accumulation was dropped, so a fresh payload starts cleanly.
			if _, done := acc.Take(6, 0, want[:3]); done {
				t.Fatal("restart reported complete after 3 of 6 bytes")
			}
			p, done := acc.Take(6, 3, want[3:])
			if !done || !bytes.Equal(p, want) {
				t.Fatalf("restart = (%v, %v), want the whole payload", p, done)
			}
		})
	}
}

// The buffer is handed over, not lent: the next payload must not write over
// bytes the caller is still holding.
func TestPayloadAccDoesNotReuseAHandedOverBuffer(t *testing.T) {
	first := payloadOf(10)
	var acc sofab.PayloadAcc
	acc.Take(len(first), 0, first[:4])
	got, done := acc.Take(len(first), 4, first[4:])
	if !done {
		t.Fatal("first payload incomplete")
	}

	second := bytes.Repeat([]byte{0xEE}, len(first))
	acc.Take(len(second), 0, second[:4])
	if _, done := acc.Take(len(second), 4, second[4:]); !done {
		t.Fatal("second payload incomplete")
	}

	if !bytes.Equal(got, first) {
		t.Fatalf("first payload = %v after a second one, want %v", got, first)
	}
}

// Reset drops a partial payload; what follows is a payload of its own.
func TestPayloadAccReset(t *testing.T) {
	want := payloadOf(5)
	var acc sofab.PayloadAcc

	acc.Take(len(want), 0, want[:2])
	acc.Reset()

	if p, done := acc.Take(len(want), 2, want[2:]); done {
		t.Fatalf("(%v, true) after Reset, want the orphaned chunk refused", p)
	}
	p, done := acc.Take(len(want), 0, want)
	if !done || !bytes.Equal(p, want) {
		t.Fatalf("(%v, %v), want the whole payload", p, done)
	}
}

// A claimed length is untrusted input: it must cost memory in proportion to the
// bytes that really arrive, not to the number announced. This is the
// amplification hardening Decoder.readRaw applies to the same claim (issue #40).
func TestPayloadAccHostileTotalAllocatesInProportion(t *testing.T) {
	chunk := payloadOf(8)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var acc sofab.PayloadAcc
	if _, done := acc.Take(math.MaxInt32, 0, chunk); done {
		t.Fatal("8 bytes reported as a 2^31-1 byte payload")
	}

	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("announcing 2^31-1 bytes allocated %d bytes, want the growth bounded", grew)
	}
}

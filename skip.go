package sofab

// Skipping a subtree the consumer declined.
//
// BeginSequence returning nil means "I have no destination for this scope"
// (§7.4 / MESSAGE_SPEC §4.9). The bytes still have to be parsed — a sequence is
// framed by markers rather than by a length, so its end has to be found — but
// nothing in it needs to be built.
//
// Two pieces do that, and both are deliberately small:
//
//   - skipV, a no-op Visitor, takes over the routing. Go's Visitor is a full
//     interface with no optional methods, so *something* has to be there; an
//     empty method is the cheapest possible something, and it keeps the two
//     accept loops unchanged.
//   - cursor.skipping / Decoder.skipping, a depth counter, takes over the
//     COST. Where a live decode would allocate — a string, an element slice —
//     the skipping path advances instead. That is the difference from the no-op
//     visitor generated code used to hand over, which decoded every value and
//     built every string before throwing it away.
//
// The receiver's caps (WithMaxStringLen and friends) do not fire inside a
// skipped subtree: they bound what this consumer is handed, and it is handed
// nothing. Format ceilings do, everywhere — arrayMax, the id ceiling, MaxDepth,
// the reserved fixlen subtypes, the exact length advance — because they bound
// what the wire may express, which is not a consumer's to waive. corelib-dart
// draws the line in the same place.
type skipVisitor struct{}

var skipV Visitor = skipVisitor{}

func (skipVisitor) Unsigned(ID, uint64) error        { return nil }
func (skipVisitor) Signed(ID, int64) error           { return nil }
func (skipVisitor) Float32(ID, float32) error        { return nil }
func (skipVisitor) Float64(ID, float64) error        { return nil }
func (skipVisitor) String(ID, string) error          { return nil }
func (skipVisitor) Bytes(ID, []byte) error           { return nil }
func (skipVisitor) UnsignedArray(ID, []uint64) error { return nil }
func (skipVisitor) SignedArray(ID, []int64) error    { return nil }
func (skipVisitor) Float32Array(ID, []float32) error { return nil }
func (skipVisitor) Float64Array(ID, []float64) error { return nil }
func (skipVisitor) EndSequence() error               { return nil }

// BeginSequence keeps a nested scope inside the skip rather than offering it
// back: nothing under a declined subtree is the consumer's, however deep.
func (skipVisitor) BeginSequence(ID) (Visitor, error) { return skipV, nil }

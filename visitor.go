package sofab

// Visitor is the ONLY decode surface this package exposes (CORELIB_PLAN
// §5.3.1): the decoder drives, calling a typed method per field. Generated code
// implements Visitor on the target struct and binds each field straight into a
// member — so a generated object can be deserialized without the caller ever
// writing a Next/Skip loop. Nested sequences descend into the visitor returned
// by BeginSequence (typically the nested generated object), which is the
// CHILD-HANDLER shape of §6.0; the flat begin/end shape is the other conformant
// one, and §6.0 names the child handler as the one Go makes cheaper.
//
// NOTHING IT HANDS OVER IS STORAGE (§6.6.3). A callback that delivered a whole
// string, a whole blob or a whole element slice would oblige the codec to build
// that value, and the only size available to build it from is the wire's — which
// is exactly what §6.6 forbids. So an aggregate arrives IN PIECES:
//
//   - a string or blob arrives as FixlenBegin (the total, before any payload
//     byte) followed by one or more String / Bytes calls, each carrying the
//     total, this piece's offset, and the piece itself;
//   - an array arrives as ArrayBegin (the kind and the element count), then one
//     ArrayUnsigned / ArraySigned / ArrayFloat32 / ArrayFloat64 call per
//     element, then ArrayEnd.
//
// The scalar callbacks are unaffected: they carry a value, and a value is not
// storage.
//
// NO VALUE OUTLIVES ITS CALLBACK (§6.7). The chunk a String or Bytes call hands
// over is a window into the bytes the CALLER fed — the codec neither allocated
// nor owns them, and asserts nothing about how long they live. A visitor that
// keeps a payload past the call copies it. This holds identically on every entry
// point: §6.7.1 gives the one-shot path no exemption, and AcceptBytes is a
// single Feed of the whole buffer, so it is not a different path.
//
// Implementing all of it is rarely necessary: embed VisitorBase (collectors.go)
// for a no-op default of every method and override only the callbacks the
// destination actually binds.
type Visitor interface {
	// Unsigned is one unsigned-integer field (§4.4). Bools arrive here too, as
	// 0/1.
	Unsigned(id ID, v uint64) error
	// Signed is one signed-integer field, already zigzag-decoded (§4.5).
	Signed(id ID, v int64) error
	// Float32 is one fp32 fixlen field, bit-exact (§6.5).
	Float32(id ID, v float32) error
	// Float64 is one fp64 fixlen field, bit-exact.
	Float64(id ID, v float64) error

	// FixlenBegin announces a fixlen field's declared subtype and byte length,
	// right after its length word is read and validated and BEFORE any payload
	// byte. It fires exactly once per fixlen field — an empty string included,
	// a float included — and never for an array element.
	//
	// It is where a schema bound on a LENGTH is enforced. MESSAGE_SPEC §5.2 has
	// INVALID dominate INCOMPLETE, so a string whose `maxlen` the length word
	// already breaches must be INVALID even when the message then ends before
	// the payload arrives; returning ErrInvalidMsg here is how a destination
	// says so, and the String callback — which never runs for such a message —
	// is too late.
	//
	// subtype is what ARRIVED, not what was declared: a destination whose field
	// expects another subtype treats this as the MESSAGE_SPEC §7.3 skip and
	// measures neither the id nor the length against that field's bound. For a
	// float, total is the fixed width (4 or 8), already validated — a malformed
	// float width is INVALID before this fires.
	FixlenBegin(id ID, subtype FixlenSubtype, total int) error
	// String delivers one piece of a string payload: total is the whole field's
	// byte length, offset is this piece's position within it, and chunk is the
	// piece. A payload that arrives whole in one fed chunk is delivered in a
	// single call with offset == 0 and len(chunk) == total; one split across fed
	// chunks arrives in as many calls as it took to feed. An empty string is one
	// call with total == 0 and an empty chunk.
	//
	// The bytes are delivered RAW: the decoder builds no string and validates no
	// UTF-8, because it cannot know whether this field has a destination at all
	// — an id the schema does not declare, or a wire type contradicting it
	// (MESSAGE_SPEC §7.3), reaches this callback exactly like a declared field,
	// and validating here would turn a mere skip INVALID (§6.4.5). Go's string
	// is a byte-container type (§6.4), so the wire bytes pass through verbatim
	// and validation belongs at the destination: it calls StringCheck.UTF8Valid
	// on the assembled payload (utf8.go). The decoder does hand the destination
	// the decode's POLICY — see StringPolicyVisitor.
	//
	// chunk aliases the caller's own fed bytes and is valid only until this call
	// returns (§6.7).
	String(id ID, total, offset int, chunk []byte) error
	// Bytes delivers one piece of a blob payload. Same chunking model, same
	// lifetime, as String; a blob is opaque and never UTF-8-checked.
	Bytes(id ID, total, offset int, chunk []byte) error

	// ArrayBegin announces an array field: the element kind the wire declares
	// and the wire element count. It fires exactly once per array field, never
	// per element, always before the first element, and count == 0 is no
	// exception (an empty array reports its kind and is followed straight by
	// ArrayEnd).
	//
	// It is the count twin of FixlenBegin, and carries the schema `count:` bound
	// for the same §5.2 reason: an over-count array that is then truncated is
	// INVALID, and returning ErrInvalidMsg here is what says so.
	//
	// WHERE IT FIRES depends on the wire type, because that is where the element
	// kind becomes known (§4.8.1):
	//
	//   - integer arrays (ArrayUnsigned, ArraySigned): right after the count
	//     varint — the wire type alone fixes the kind, there is no second word;
	//   - fixlen arrays (ArrayFp32, ArrayFp64): after the fixlen_word, once the
	//     element subtype is read and found format-legal. The count word is read
	//     first, and the FORMAT ceiling and any receiver cap still fire there,
	//     but the announcement is deferred so the kind it carries is never a
	//     guess.
	//
	// The deferral is what MESSAGE_SPEC §7.3 requires: a fixlen array whose
	// subtype contradicts the declared element type is skipped, and the schema
	// count bound MUST NOT be applied to it — the field was never this array's
	// value. A consequence, and intended: a message that ends between the two
	// words is INCOMPLETE, not INVALID, because no bound can yet be judged. A
	// fixlen_word that is format-illegal (a string or blob subtype, or a width
	// mismatch) is INVALID before this fires; that is a format violation (§4.8),
	// not a skippable schema mismatch.
	ArrayBegin(id ID, kind ArrayKind, count int) error
	// ArrayUnsigned is one element of an unsigned integer array, at index.
	//
	// The DECLARED ELEMENT WIDTH is enforced here. The elements go past one at a
	// time, so a value outside the width its schema declares is rejected the
	// moment it is read — which is what keeps it INVALID rather than INCOMPLETE
	// when the array is then truncated (§5.2, generator#267). A whole-slice
	// callback could not: an array that never arrives never reaches it.
	ArrayUnsigned(id ID, index int, v uint64) error
	// ArraySigned is one element of a signed integer array, already
	// zigzag-decoded. See ArrayUnsigned for the width bound.
	ArraySigned(id ID, index int, v int64) error
	// ArrayFloat32 is one element of an fp32 array, bit-exact.
	ArrayFloat32(id ID, index int, v float32) error
	// ArrayFloat64 is one element of an fp64 array, bit-exact.
	ArrayFloat64(id ID, index int, v float64) error
	// ArrayEnd closes the array ArrayBegin opened, once its declared count of
	// elements has been delivered. It does NOT fire for an array the message
	// truncates: the count was never reached.
	ArrayEnd(id ID) error

	// BeginSequence returns the visitor that receives the nested scope's fields,
	// or nil to SKIP the scope: nothing in it is delivered, nothing under it is
	// offered again however deep, and no EndSequence fires for it.
	//
	// Skipping is what a consumer says about a subtree it has no destination for
	// — an unknown id, a field it does not care about. The bytes are still parsed
	// (a sequence is framed by markers, not by a length, so its end has to be
	// found) but nothing in it is delivered: no piece is reported, no element
	// announced.
	//
	// Nothing inside a skipped scope is capped: a receiver cap (§6.2.1) lives in
	// a destination callback, no callback fires here, and the section says the
	// same from the other end — a cap bounds what this consumer is handed, and
	// it is handed nothing. Format ceilings still fire, everywhere.
	BeginSequence(id ID) (Visitor, error)
	// EndSequence is called on that nested visitor once its scope closes, so a
	// generated nested object can finalize itself.
	EndSequence() error
}

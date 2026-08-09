package sofab

import "errors"

// ID is a field identifier. Application-assigned; need not be contiguous.
type ID uint32

// IDMax is the largest valid field id (INT32_MAX), matching SOFAB_ID_MAX in C.
const IDMax ID = 0x7FFF_FFFF

// APIVersion is the SofaBuffers wire-format API version implemented by this
// package. It matches API_VERSION in the language-neutral specification and is
// used by callers and the code generator to check compatibility.
const APIVersion = 1

// arrayMax bounds array element counts and fixlen byte lengths (INT32_MAX).
const arrayMax uint64 = 0x7FFF_FFFF

// MaxDepth is the maximum nested-sequence depth (CORELIB_PLAN §4.9/§6.2). An
// encoder must not open more than MaxDepth nested sequences, and a decoder
// rejects a message that nests deeper with ErrInvalidMsg rather than risk
// unbounded recursion / stack growth.
const MaxDepth = 255

// WireType is the 3-bit field type tag in the low bits of a field header.
type WireType uint8

const (
	TypeVarintUnsigned      WireType = 0x0 // unsigned varint
	TypeVarintSigned        WireType = 0x1 // zigzag + varint
	TypeFixlen              WireType = 0x2 // fp/string/blob, length-prefixed
	TypeVarintArrayUnsigned WireType = 0x3 // count + unsigned varints
	TypeVarintArraySigned   WireType = 0x4 // count + zigzag varints
	TypeFixlenArray         WireType = 0x5 // count + elem header + raw elements
	TypeSequenceStart       WireType = 0x6 // open nested sequence
	TypeSequenceEnd         WireType = 0x7 // close nested sequence
)

// fixlen subtypes (3-bit tag inside the fixlen header).
const (
	fixFp32 uint64 = 0x0
	fixFp64 uint64 = 0x1
	fixStr  uint64 = 0x2
	fixBlob uint64 = 0x3
)

// ArrayKind names the element kind an array header on the wire declares, as
// delivered to HeaderVisitor.ArrayBegin. It distinguishes the two fixlen
// element subtypes — fp32 and fp64 — rather than collapsing them, because for a
// fixlen array (§4.8) the element subtype decides whether the field is this
// array's value at all: a header whose subtype contradicts the declared element
// type is skipped under MESSAGE_SPEC §7.3, and the schema count bound must not
// be applied to it. Generated code therefore keys its bound on the kind and
// applies it only inside the arm matching the declared element type.
//
// The ordinals are normative and shared by every push-API corelib in the family
// (they match corelib-ts src/constants.ts ArrayKind). The Go names carry an
// Array prefix because the bare Unsigned/Signed identifiers are already taken by
// this package's element-type constraints; the values are unchanged.
type ArrayKind uint8

const (
	ArrayUnsigned ArrayKind = 0 // wire type 0b011, count + unsigned varints
	ArraySigned   ArrayKind = 1 // wire type 0b100, count + zigzag varints
	ArrayFp32     ArrayKind = 2 // wire type 0b101, fixlen_word subtype fp32 / 4 B
	ArrayFp64     ArrayKind = 3 // wire type 0b101, fixlen_word subtype fp64 / 8 B
)

// Field is a decoded field header returned by Decoder.Next.
//
// On a TypeSequenceEnd marker ID is always 0, whatever id the header on the
// wire spelled: the end marker's id carries no information, so the decoder
// discards it (§4.9) instead of handing a sender-chosen number to a caller that
// switches on ID.
type Field struct {
	ID   ID
	Type WireType
}

// Unsigned constrains integer element types accepted by the unsigned array
// helpers.
type Unsigned interface {
	~uint8 | ~uint16 | ~uint32 | ~uint64
}

// Signed constrains integer element types accepted by the signed array helpers.
type Signed interface {
	~int8 | ~int16 | ~int32 | ~int64
}

// Errors returned by the encoder and decoder. They mirror the C sofab_ret_t
// codes (§6.3).
var (
	// ErrBufferFull is the BufferFull code (§6.3): an output buffer the caller
	// supplied (NewEncoderBuffer, or SetBuffer) ran out of room with no flush
	// sink to drain it to. The encode stops there and the error is sticky, so a
	// partial message is never reported as a complete one; nothing is grown or
	// reallocated, because a caller-supplied buffer is what it is (§5.1).
	//
	// It cannot arise on an encoder with a sink — a full buffer is flushed and
	// written on into — nor on the io.Writer form, whose window is this package's
	// own to grow; there a failed write surfaces as the io.Writer's own error.
	ErrBufferFull = errors.New("sofab: output buffer full")
	// ErrArgument is an invalid caller argument (e.g. id > IDMax, opening a
	// nested sequence past MaxDepth, calling a typed pull reader when no field's
	// value is waiting to be read, or — with strict UTF-8 enabled, §6.4 — a
	// WriteString value that is not valid UTF-8). This is the InvalidArgument code
	// (§6.3), and since §6.3 removed the separate "invalid usage" code it is the
	// only code for a caller mistake.
	ErrArgument = errors.New("sofab: invalid argument")
	// ErrTypeMismatch reports that a typed pull read did not match the field on
	// the wire — a different wire type, or for a fixlen a different subtype than
	// the one the requested type maps to. Per MESSAGE_SPEC §7.3 this is *not an
	// error about the message*: the field is skipped exactly like one with an
	// unknown id, the destination is left untouched, and a decode that meets
	// nothing else stays COMPLETE. The same bytes decode fine for a peer whose
	// schema declares the other type, so reporting them as malformed would turn
	// the forward-compatibility case §7.3 exists for into a hard failure.
	//
	// It is therefore a distinct sentinel, deliberately neither ErrInvalidMsg nor
	// ErrArgument: a mismatch is not a caller mistake either, because nothing at
	// the call distinguishes "the caller bound the wrong type" from "a peer sent
	// another type for this id" — and §6.3 says the outcome is the same skip
	// regardless.
	//
	// The value is consumed before the error is returned, so the decoder sits on
	// the next field boundary and a pull loop simply continues with Next. The one
	// exception is a sequence start/end, which carries no value: there nothing is
	// consumed and the caller skips the sub-tree with Skip, exactly as it does for
	// an unknown id.
	//
	// Test for it with errors.Is. Generated code decodes through Accept /
	// AcceptBytes and applies §7.3 in its own field switch, so it never sees this.
	ErrTypeMismatch = errors.New("sofab: field type does not match the read")
	// ErrInvalidMsg is malformed input that is wrong regardless of what bytes
	// might follow: varint overflow (> 64 bits), a bad type/subtype tag, a length
	// or count past arrayMax, a dangling sequence end, nesting past MaxDepth, or
	// invalid UTF-8 in a string. This is the INVALID decode outcome
	// (MESSAGE_SPEC §7).
	ErrInvalidMsg = errors.New("sofab: invalid message")
	// ErrIncomplete is the INCOMPLETE decode outcome (MESSAGE_SPEC §7): the input
	// ended *inside* a field — a varint whose continuation bit was set with no
	// terminating byte, a fixlen/array payload shorter than its declared length,
	// or a nested sequence that was never closed. The bytes so far are valid but
	// do not form a complete message; feeding more could complete it.
	//
	// INCOMPLETE is NOT a malformed-message error. Like io.EOF, it is an outcome
	// surfaced as a sentinel: the decoder does not itself decide that a trailing
	// incomplete field is fatal — the caller owns end-of-input and judges, from
	// its own framing (length prefix, datagram boundary, EOF), whether a trailing
	// ErrIncomplete is a truncation error. Test for it with errors.Is; it is
	// distinct from ErrInvalidMsg so a truncated stream is never conflated with a
	// malformed one.
	ErrIncomplete = errors.New("sofab: incomplete message")
	// ErrLimitExceeded is returned when a decoded field exceeds a limit configured
	// on the decoder via WithMaxArrayCount, WithMaxStringLen, or WithMaxBlobLen. It
	// is a *receiver-side policy* decision, not a property of the wire format: the
	// bytes may be perfectly well-formed, but a locally configured cap rejects
	// them. It is therefore a distinct sentinel from ErrInvalidMsg — a message
	// turned away only because it exceeds a locally chosen limit must never be
	// conflated with a malformed one (e.g. differential fuzzing must not read a
	// limit rejection as a conformance divergence). Test for it with errors.Is.
	// With no limits configured (the default) it is never returned.
	ErrLimitExceeded = errors.New("sofab: decode limit exceeded")
)

// zigzagEncode maps a signed value to its unsigned varint representation.
func zigzagEncode(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

// zigzagDecode reverses zigzagEncode.
func zigzagDecode(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

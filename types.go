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

// Field is a decoded field header returned by Decoder.Next.
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
// codes (write-buffer-full is reported via the underlying io.Writer instead).
var (
	// ErrArgument is an invalid caller argument (e.g. id > IDMax, or opening a
	// nested sequence past MaxDepth).
	ErrArgument = errors.New("sofab: invalid argument")
	// ErrUsage is invalid API usage (e.g. reading a value as the wrong type).
	ErrUsage = errors.New("sofab: invalid usage")
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
)

// zigzagEncode maps a signed value to its unsigned varint representation.
func zigzagEncode(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

// zigzagDecode reverses zigzagEncode.
func zigzagDecode(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

package sofab

import "errors"

// ID is a field identifier. Application-assigned; need not be contiguous.
type ID uint32

// IDMax is the largest valid field id (INT32_MAX), matching SOFAB_ID_MAX in C.
const IDMax ID = 0x7FFF_FFFF

// arrayMax bounds array element counts and fixlen byte lengths (INT32_MAX).
const arrayMax uint64 = 0x7FFF_FFFF

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
	// ErrArgument is an invalid caller argument (e.g. id > IDMax, empty array).
	ErrArgument = errors.New("sofab: invalid argument")
	// ErrUsage is invalid API usage (e.g. reading a value as the wrong type).
	ErrUsage = errors.New("sofab: invalid usage")
	// ErrInvalidMsg is malformed input (varint overflow, bad type tag,
	// zero-length array, truncated payload, dangling sequence end, ...).
	ErrInvalidMsg = errors.New("sofab: invalid message")
)

// zigzagEncode maps a signed value to its unsigned varint representation.
func zigzagEncode(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

// zigzagDecode reverses zigzagEncode.
func zigzagDecode(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

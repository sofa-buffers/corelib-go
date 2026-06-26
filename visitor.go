package sofab

import (
	"encoding/binary"
	"io"
	"math"
)

// Visitor is the push/visitor counterpart to the pull parser (Decoder.Next):
// the decoder drives, calling a typed method per field. Generated code
// implements Visitor on the target struct and binds each field straight into a
// member — so a generated object can be deserialized without the caller ever
// writing a Next/Skip loop. Nested sequences descend into the visitor returned
// by BeginSequence (typically the nested generated object).
//
// Array methods receive the values widened to the 64-bit value domain (or the
// concrete float slice); the generated code narrows to its declared element
// width.
type Visitor interface {
	Unsigned(id ID, v uint64) error
	Signed(id ID, v int64) error
	Float32(id ID, v float32) error
	Float64(id ID, v float64) error
	String(id ID, s string) error
	Bytes(id ID, b []byte) error
	UnsignedArray(id ID, v []uint64) error
	SignedArray(id ID, v []int64) error
	Float32Array(id ID, v []float32) error
	Float64Array(id ID, v []float64) error
	// BeginSequence returns the visitor that receives the nested scope's fields.
	BeginSequence(id ID) (Visitor, error)
	// EndSequence is called on that nested visitor once its scope closes, so a
	// generated nested object can finalize itself.
	EndSequence() error
}

// Accept drives the decoder over the entire top-level stream, dispatching each
// field to v. It returns nil at a clean end of stream, or a malformed-message
// error on bad input.
func (d *Decoder) Accept(v Visitor) error {
	return d.accept(v, false)
}

func (d *Decoder) accept(v Visitor, nested bool) error {
	for {
		h, err := d.readVarint(true)
		if err != nil {
			if err == io.EOF {
				if nested {
					return ErrInvalidMsg // missing sequence end
				}
				return nil
			}
			return err
		}
		t := WireType(h & 0x07)
		id := ID(h >> 3)
		if t != TypeSequenceEnd && (h>>3) > uint64(IDMax) {
			return ErrInvalidMsg
		}
		switch t {
		case TypeVarintUnsigned:
			x, err := d.readVarint(false)
			if err != nil {
				return err
			}
			if err := v.Unsigned(id, x); err != nil {
				return err
			}
		case TypeVarintSigned:
			x, err := d.readVarint(false)
			if err != nil {
				return err
			}
			if err := v.Signed(id, zigzagDecode(x)); err != nil {
				return err
			}
		case TypeFixlen:
			if err := d.acceptFixlen(v, id); err != nil {
				return err
			}
		case TypeVarintArrayUnsigned:
			n, err := d.arrayCount()
			if err != nil {
				return err
			}
			out := make([]uint64, n)
			for i := range out {
				if out[i], err = d.readVarint(false); err != nil {
					return err
				}
			}
			if err := v.UnsignedArray(id, out); err != nil {
				return err
			}
		case TypeVarintArraySigned:
			n, err := d.arrayCount()
			if err != nil {
				return err
			}
			out := make([]int64, n)
			for i := range out {
				x, err := d.readVarint(false)
				if err != nil {
					return err
				}
				out[i] = zigzagDecode(x)
			}
			if err := v.SignedArray(id, out); err != nil {
				return err
			}
		case TypeFixlenArray:
			if err := d.acceptFixlenArray(v, id); err != nil {
				return err
			}
		case TypeSequenceStart:
			child, err := v.BeginSequence(id)
			if err != nil {
				return err
			}
			if err := d.accept(child, true); err != nil {
				return err
			}
			if err := child.EndSequence(); err != nil {
				return err
			}
		case TypeSequenceEnd:
			if nested {
				return nil
			}
			return ErrInvalidMsg // dangling end at top level
		default:
			return ErrInvalidMsg
		}
	}
}

func (d *Decoder) acceptFixlen(v Visitor, id ID) error {
	n, sub, err := d.readFixlenHeader()
	if err != nil {
		return err
	}
	switch sub {
	case fixFp32:
		if n != 4 {
			return ErrInvalidMsg
		}
		buf, err := d.readRaw(4)
		if err != nil {
			return err
		}
		return v.Float32(id, math.Float32frombits(binary.LittleEndian.Uint32(buf)))
	case fixFp64:
		if n != 8 {
			return ErrInvalidMsg
		}
		buf, err := d.readRaw(8)
		if err != nil {
			return err
		}
		return v.Float64(id, math.Float64frombits(binary.LittleEndian.Uint64(buf)))
	case fixStr:
		buf, err := d.readRaw(n)
		if err != nil {
			return err
		}
		return v.String(id, string(buf))
	case fixBlob:
		buf, err := d.readRaw(n)
		if err != nil {
			return err
		}
		return v.Bytes(id, buf)
	default:
		return ErrInvalidMsg
	}
}

func (d *Decoder) acceptFixlenArray(v Visitor, id ID) error {
	n, err := d.arrayCount()
	if err != nil {
		return err
	}
	h, err := d.readVarint(false)
	if err != nil {
		return err
	}
	sub := h & 0x07
	size := h >> 3
	switch {
	case sub == fixFp32 && size == 4:
		out := make([]float32, n)
		for i := range out {
			buf, err := d.readRaw(4)
			if err != nil {
				return err
			}
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf))
		}
		return v.Float32Array(id, out)
	case sub == fixFp64 && size == 8:
		out := make([]float64, n)
		for i := range out {
			buf, err := d.readRaw(8)
			if err != nil {
				return err
			}
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(buf))
		}
		return v.Float64Array(id, out)
	default:
		return ErrInvalidMsg
	}
}

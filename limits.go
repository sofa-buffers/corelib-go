package sofab

// Option configures optional decode-time limits. Options are passed to
// NewDecoder (covering the pull parser and Decoder.Accept) or to AcceptBytes.
//
// With no options the decoder enforces no limits and behaves bit-for-bit as it
// did before limits existed. Limits are strictly opt-in: this package invents no
// default cap. (The amplification hardening in issue #40's Part A is separate and
// unconditional — it applies with or without any Option, because it never
// changes which messages decode, only how eagerly memory is allocated.)
//
// A limit is a receiver-side policy, not a wire-format rule. Exceeding one is
// reported as ErrLimitExceeded, deliberately distinct from ErrInvalidMsg, so a
// message rejected purely for exceeding a locally configured cap is never
// conflated with a malformed message. Limits are enforced at header time —
// before any payload is buffered or any element slice is allocated — so an
// oversize claim is rejected even if the payload bytes never arrive. A limit is
// never clamped or truncated: the field is rejected.
//
// Limits apply per field occurrence: array element count for count-prefixed
// arrays, and byte length for strings and blobs.
type Option func(*limits)

// limits holds the optional per-field decode caps. The zero value (every field
// 0) means unlimited, which is the default.
type limits struct {
	maxArrayCount uint64 // 0 = unlimited
	maxStringLen  uint64 // 0 = unlimited
	maxBlobLen    uint64 // 0 = unlimited
}

// WithMaxArrayCount caps the element count of every count-prefixed array — the
// unsigned, signed, and fixlen (fp32/fp64) array types. A message whose array
// claims more than n elements is rejected with ErrLimitExceeded at the count
// header, before any element slice is allocated. A non-positive n leaves the
// limit unset (unlimited).
func WithMaxArrayCount(n int) Option {
	return func(l *limits) { l.maxArrayCount = clampLimit(n) }
}

// WithMaxStringLen caps the byte length of every string field. A string whose
// length header exceeds n bytes is rejected with ErrLimitExceeded before the
// payload is read. A non-positive n leaves the limit unset (unlimited).
func WithMaxStringLen(n int) Option {
	return func(l *limits) { l.maxStringLen = clampLimit(n) }
}

// WithMaxBlobLen caps the byte length of every blob field. A blob whose length
// header exceeds n bytes is rejected with ErrLimitExceeded before the payload is
// read. A non-positive n leaves the limit unset (unlimited).
func WithMaxBlobLen(n int) Option {
	return func(l *limits) { l.maxBlobLen = clampLimit(n) }
}

// clampLimit maps a caller-supplied limit to its internal form: a non-positive
// value means "no limit" (stored as 0).
func clampLimit(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n)
}

// newLimits folds the options into a limits value.
func newLimits(opts []Option) limits {
	var l limits
	for _, opt := range opts {
		opt(&l)
	}
	return l
}

// checkArrayCount enforces maxArrayCount. The caller has already range-checked n
// against arrayMax.
func (l limits) checkArrayCount(n uint64) error {
	if l.maxArrayCount != 0 && n > l.maxArrayCount {
		return ErrLimitExceeded
	}
	return nil
}

// checkFixlen enforces the string/blob byte-length limits at the fixlen header,
// before the payload is buffered. The fp32/fp64 subtypes carry no configurable
// limit (their length is fixed at 4/8 bytes).
func (l limits) checkFixlen(sub, length uint64) error {
	switch sub {
	case fixStr:
		if l.maxStringLen != 0 && length > l.maxStringLen {
			return ErrLimitExceeded
		}
	case fixBlob:
		if l.maxBlobLen != 0 && length > l.maxBlobLen {
			return ErrLimitExceeded
		}
	}
	return nil
}

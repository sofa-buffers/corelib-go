package sofab

// Utf8Valid is the pre-rename spelling of UTF8Valid (#116), kept as a forward.
//
// Deprecated: use UTF8Valid.
//
// It survives the rename because generated code from sofa-buffers/generator
// still emits sofab.Utf8Valid, and the generator's CI builds that generated
// code against this repository's main: dropping the old name in the same change
// that introduces the new one would turn that CI red until the generator's own
// switch lands. A follow-up on #116 removes both forwards once it has.
//
// The forwards carry no build tag of their own. They call UTF8Valid, which is
// declared in utf8.go and therefore exists in the default build and in a
// `sofab_no_strict_utf8` one alike, so a single copy serves both configurations
// and the two cannot drift apart.
func Utf8Valid(b []byte) bool { return UTF8Valid(b) }

// Utf8Valid is the pre-rename spelling of StringCheck.UTF8Valid (#116).
//
// Deprecated: use StringCheck.UTF8Valid. It is here for the same reason as the
// package-level Utf8Valid above — generated code still calls it — and goes away
// together with it.
func (c StringCheck) Utf8Valid(b []byte) bool { return c.UTF8Valid(b) }

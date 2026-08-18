package sofab_test

import (
	"testing"

	sofab "github.com/sofa-buffers/corelib-go"
)

// TestDeprecatedUtf8ValidForwards pins the only property the pre-rename names
// owe: they answer exactly what the new ones answer, in whichever build runs.
// Generated code still calls them, so a forward that drifted — or that was
// accidentally given a build tag and so existed in only one configuration —
// would change the validation behaviour of every generated destination arm
// without any call site changing.
func TestDeprecatedUtf8ValidForwards(t *testing.T) {
	cases := map[string][]byte{
		"empty":        {},
		"ascii":        []byte("abc"),
		"embedded NUL": {'a', 0x00, 'b'},
		"two-byte":     []byte("ä"),
		"lone 0xFF":    {0xFF},
		"overlong NUL": {0xC0, 0x80},
		"surrogate":    {0xED, 0xA0, 0x80},
		"truncated":    {0xE2, 0x82},
	}

	var zero sofab.StringCheck
	for name, b := range cases {
		if got, want := sofab.Utf8Valid(b), sofab.UTF8Valid(b); got != want {
			t.Errorf("%s: Utf8Valid = %v, UTF8Valid = %v", name, got, want)
		}
		if got, want := zero.Utf8Valid(b), zero.UTF8Valid(b); got != want {
			t.Errorf("%s: StringCheck.Utf8Valid = %v, StringCheck.UTF8Valid = %v", name, got, want)
		}
	}
}

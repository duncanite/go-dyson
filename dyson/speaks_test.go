package dyson_test

import (
	"errors"
	"testing"

	"github.com/duncanite/go-dyson/dyson"
)

// TestSpeaks: the types this package speaks are the Link's and the
// Hot+Cool's with its variants, and Speaks says of each type what Dial
// does with it: a type it does not speak is the one Dial refuses as
// unsupported, before anything else it is given is looked at.
func TestSpeaks(t *testing.T) {
	t.Parallel()

	for product, want := range map[string]bool{
		dyson.PureCoolLink: true, dyson.PureHotCool: true, "527E": true, "527K": true, "527M": true,
		"": false, "455": false, "438": false, "527X": false, "4750": false, "527e": false,
	} {
		if got := dyson.Speaks(product); got != want {
			t.Errorf("Speaks(%q) = %t, want %t", product, got, want)
		}

		// With no host, a type that is spoken is refused for the host, and
		// one that is not for itself.
		_, err := dyson.Dial(t.Context(), "", dyson.Identity{Serial: "AB1-EU-ABC1234A", Type: product, Credential: "c"})
		if unsupported := errors.Is(err, dyson.ErrUnsupported); unsupported == want {
			t.Errorf("Dial of the type %q = %v, while Speaks says %t", product, err, want)
		}
	}
}

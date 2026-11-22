package dyson_test

// The contract of finding a purifier, from libdyson-neon's discovery
// (https://github.com/libdyson-wg/libdyson-neon, libdyson/discovery.py and
// its test): a fan announces the service _dyson_mqtt._tcp, and the first
// label of its instance's name is its product type, an underscore, and its
// serial: "475_NK6-CN-HAA0000A". opendyson reads the same name, for the
// part after the underscore.

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/dysontest"
)

// TestRecognize: an instance named by a product type and a serial is a
// purifier's, with the values its sticker gives and the addresses that are
// some; anything else is none.
func TestRecognize(t *testing.T) {
	t.Parallel()

	here := netip.MustParseAddrPort("192.0.2.10:1883")

	for _, row := range []struct {
		name     string
		instance string
		at       []netip.AddrPort
		want     dyson.Found
		is       bool
	}{
		{
			"a Pure Cool Link", "475_NK6-CN-HAA0000A",
			[]netip.AddrPort{here},
			dyson.Found{Type: "475", Serial: "NK6-CN-HAA0000A", Addresses: []netip.AddrPort{here}},
			true,
		},
		{
			"its name whole, the service and the domain after it", "475_NK6-CN-HAA0000A._dyson_mqtt._tcp.local.", nil,
			dyson.Found{Type: "475", Serial: "NK6-CN-HAA0000A", Addresses: []netip.AddrPort{}},
			true,
		},
		{
			"a type this package does not speak", "438E_NK6-CN-HAA0000A", nil,
			dyson.Found{Type: "438E", Serial: "NK6-CN-HAA0000A", Addresses: []netip.AddrPort{}},
			true,
		},
		{
			"an address that is none is left out", "475_NK6-CN-HAA0000A",
			[]netip.AddrPort{{}, here},
			dyson.Found{Type: "475", Serial: "NK6-CN-HAA0000A", Addresses: []netip.AddrPort{here}},
			true,
		},
		{"a robot vacuum's name", "360EYE-NK6-CN-HAA0000A", nil, dyson.Found{}, false},
		{"a serial alone", "NK6-CN-HAA0000A", nil, dyson.Found{}, false},
		{"a type alone", "475_", nil, dyson.Found{}, false},
		{"no type", "_NK6-CN-HAA0000A", nil, dyson.Found{}, false},
		{"a type that is none", "47_NK6-CN-HAA0000A", nil, dyson.Found{}, false},
		{"a serial that is none", "475_NK6-CN-HAA", nil, dyson.Found{}, false},
		{"a serial in small letters", "475_nk6-cn-haa0000a", nil, dyson.Found{}, false},
		{"more before the type", "x475_NK6-CN-HAA0000A", nil, dyson.Found{}, false},
		{"more after the serial", "475_NK6-CN-HAA0000A_x", nil, dyson.Found{}, false},
		{"a line's end in it", "475_NK6-CN-HAA0000A\n", nil, dyson.Found{}, false},
		{"another maker's instance", "Living Room", nil, dyson.Found{}, false},
		{"nothing", "", nil, dyson.Found{}, false},
	} {
		found, is := dyson.Recognize(dyson.Advertisement{Instance: row.instance, Addresses: row.at})
		if is != row.is || !reflect.DeepEqual(found, row.want) {
			t.Errorf("%s: Recognize = %+v, %t, want %+v, %t", row.name, found, is, row.want, row.is)
		}
	}
}

// TestRecognizeTheStickers: what an announcement says of a purifier is what
// its sticker says: the serial and the type FromSticker reads, and an
// address Dial takes.
func TestRecognizeTheStickers(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	identity, err := dyson.FromSticker(dysontest.SSID, dysontest.WiFiPassword)
	if err != nil {
		t.Fatalf("FromSticker: %v", err)
	}

	found, is := dyson.Recognize(dyson.Advertisement{
		Instance:  identity.Type + "_" + identity.Serial,
		Addresses: []netip.AddrPort{netip.MustParseAddrPort(fake.Addr())},
	})
	if !is || found.Serial != identity.Serial || found.Type != identity.Type || len(found.Addresses) != 1 {
		t.Fatalf("Recognize of the test purifier's announcement = %+v, %t", found, is)
	}

	purifier, err := dyson.Dial(soon(t), found.Addresses[0].String(), identity)
	if err != nil {
		t.Fatalf("Dial at the address found: %v", err)
	}

	if err = purifier.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// errDeaf is what the browser of TestFind ends with.
var errDeaf = errors.New("the network hears no more")

// heard is a browser that tells what it was given, then ends.
type heard struct {
	service string
	tells   []dyson.Advertisement
	lost    []bool
}

func (h *heard) Browse(_ context.Context, service string, handle func(dyson.Advertisement, bool)) error {
	h.service = service

	for index, advertisement := range h.tells {
		handle(advertisement, h.lost[index])
	}

	return errDeaf
}

// TestFind: Find browses for the purifiers' service, tells of each
// instance that is a purifier's, found or lost, and of no other, and ends
// as the browser does; with no browser or no handler nothing is asked.
func TestFind(t *testing.T) {
	t.Parallel()

	here := netip.MustParseAddrPort("192.0.2.10:1883")
	browser := &heard{
		tells: []dyson.Advertisement{
			{Instance: "475_NK6-CN-HAA0000A", Addresses: []netip.AddrPort{here}},
			{Instance: "Living Room"},
			{Instance: "475_NK6-CN-HAA0000A", Addresses: []netip.AddrPort{here}},
			{Instance: "475_NK6-CN-HAA0000A"},
		},
		lost: []bool{false, false, false, true},
	}

	type told struct {
		found dyson.Found
		lost  bool
	}

	var got []told

	err := dyson.Find(t.Context(), browser, func(found dyson.Found, lost bool) {
		got = append(got, told{found, lost})
	})

	purifier := dyson.Found{Type: "475", Serial: "NK6-CN-HAA0000A", Addresses: []netip.AddrPort{here}}
	want := []told{
		{purifier, false},
		{purifier, false},
		{dyson.Found{Type: "475", Serial: "NK6-CN-HAA0000A", Addresses: []netip.AddrPort{}}, true},
	}

	if !errors.Is(err, errDeaf) || browser.service != dyson.Service || !reflect.DeepEqual(got, want) {
		t.Errorf("Find browsed for %q, told %+v and ended with %v", browser.service, got, err)
	}

	if err = dyson.Find(t.Context(), nil, func(dyson.Found, bool) {}); !errors.Is(err, dyson.ErrArgument) {
		t.Errorf("Find with no browser = %v, want ErrArgument", err)
	}

	unasked := &heard{}
	if err = dyson.Find(t.Context(), unasked, nil); !errors.Is(err, dyson.ErrArgument) || unasked.service != "" {
		t.Errorf("Find with no handler = %v, having browsed for %q", err, unasked.service)
	}
}

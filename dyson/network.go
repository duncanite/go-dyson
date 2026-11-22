package dyson

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Service is the service a purifier announces itself as on the local
// network, over DNS-SD, and what a search for purifiers asks for.
const Service = "_dyson_mqtt._tcp"

// Advertisement is one instance of a service as it was heard on the
// network: what this package reads of it, and nothing of the means that
// heard it.
type Advertisement struct {
	// Instance is the instance's name: its first label, "475_NN2-EU-…",
	// with or without the service and the domain after it.
	Instance string
	// Addresses is where it was heard to be, each an address and the port
	// its service is on.
	Addresses []netip.AddrPort
}

// Browser is the means of finding services on the local network, which this
// package does not have and is lent: mDNS's port is one to a host, and it
// is the program's. Whatever browses satisfies it with a few lines.
type Browser interface {
	// Browse tells handle of the instances of a service: each one it
	// knows of or hears, and each change to one, whole as it then stands,
	// with lost false; and each one that is gone, as it last stood, with
	// lost true. Calls come one at a time and in order, for as long as
	// ctx lasts. It returns ctx's error when ctx ends, and its own when
	// it can browse no more.
	//
	// One that is gone has to come with its name: Find knows a purifier
	// by it.
	Browse(ctx context.Context, service string, handle func(advertisement Advertisement, lost bool)) error
}

// Found is what a device on the network says of itself: that it is a Dyson
// purifier, which one, and where. Its type may be one this package does
// not speak: Dial says so.
//
// An announcement is anyone's to make, and proves nothing. What opens a
// purifier is its credential, which is not on the network: it is the
// sticker's, and a device that is not the purifier does not have it.
type Found struct {
	// Serial and Type are who it says it is: the values FromSticker reads
	// off that purifier's sticker.
	Serial string
	Type   string
	// Addresses is where it says it is, in the order they came, each as
	// Dial takes it once written out: none when the announcement had
	// none. Which of several reaches the purifier from here is the
	// caller's to know, or to try.
	Addresses []netip.AddrPort
}

// announced is the name of a purifier's instance: its product type, then
// its serial, as the reference implementation's discovery reads it.
var announced = regexp.MustCompile(`^(\d{3}[A-Z]?)_([0-9A-Z]{3}-[A-Z]{2}-[0-9A-Z]{8,})$`)

// Recognize says whether an instance of Service is a Dyson purifier's, and
// which purifier: its name is the purifier's product type and its serial.
func Recognize(advertisement Advertisement) (Found, bool) {
	label, _, _ := strings.Cut(advertisement.Instance, ".")

	parts := announced.FindStringSubmatch(label)
	if parts == nil {
		return Found{}, false
	}

	found := Found{Type: parts[1], Serial: parts[2], Addresses: make([]netip.AddrPort, 0, len(advertisement.Addresses))}

	for _, address := range advertisement.Addresses {
		if address.IsValid() {
			found.Addresses = append(found.Addresses, address)
		}
	}

	return found, true
}

// Find tells handle of the Dyson purifiers on the local network, through
// the browser it is lent: each one found, and again each time the browser
// tells of it again, whether or not what Found holds changed, with lost
// false; each one gone, with lost true. It is the browser's Browse for
// Service, narrowed to what Recognize takes, with nothing kept between two
// calls, and lasts and ends as that does.
func Find(ctx context.Context, browser Browser, handle func(found Found, lost bool)) error {
	if browser == nil || handle == nil {
		return fmt.Errorf("%w: Find needs a browser and a handler", ErrArgument)
	}

	return browser.Browse(ctx, Service, func(advertisement Advertisement, lost bool) {
		if found, ok := Recognize(advertisement); ok {
			handle(found, lost)
		}
	})
}

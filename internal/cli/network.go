package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"text/tabwriter"

	mdns "github.com/forkcloser/go-mdns"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/internal/terminal"
	"github.com/duncanite/go-dyson/mqtt"
)

// errNotFound is a purifier that did not answer a search.
var errNotFound = errors.New("not found")

// lent is the host's mDNS as the library asks for it.
type lent struct {
	peer *mdns.Peer
}

func (l lent) Browse(ctx context.Context, service string, handle func(dyson.Advertisement, bool)) error {
	//nolint:wrapcheck // the Peer's own word, which the library hands on as it is
	return l.peer.Browse(ctx, mdns.Query{Type: service}, func(change mdns.Change, heard mdns.Instance) {
		advertisement := dyson.Advertisement{
			Instance:  heard.Name,
			Addresses: make([]netip.AddrPort, 0, len(heard.Addrs)),
		}

		for _, addr := range heard.Addrs {
			advertisement.Addresses = append(advertisement.Addresses, netip.AddrPortFrom(addr, heard.Port))
		}

		handle(advertisement, change == mdns.Lost)
	})
}

// search finds the purifiers that announce themselves within the wait, in
// the order of their serials. It ends sooner once one is enough.
func (root *rootCommand) search(ctx context.Context, enough func(dyson.Found) bool) ([]dyson.Found, error) {
	if root.Wait <= 0 {
		return nil, fmt.Errorf("%w: --wait wants a time above zero", errUsage)
	}

	cfg := mdns.Config{Network: root.network}

	if root.Interface != "" {
		cfg.Interfaces = mdns.On(root.Interface)
	}

	peer, err := mdns.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the network: %w", err)
	}

	defer func() { _ = peer.Close() }()

	searching, cancel := context.WithTimeout(ctx, root.Wait)
	defer cancel()

	found := make(map[string]dyson.Found)
	sated := false

	err = dyson.Find(searching, lent{peer: peer}, func(purifier dyson.Found, lost bool) {
		if lost {
			delete(found, purifier.Serial)

			return
		}

		found[purifier.Serial] = purifier

		if enough != nil && enough(purifier) {
			sated = true

			cancel()
		}
	})

	if ctx.Err() != nil {
		return nil, fmt.Errorf("searching for purifiers: %w", context.Cause(ctx))
	}

	if !sated && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("searching for purifiers: %w", err)
	}

	purifiers := make([]dyson.Found, 0, len(found))
	for _, serial := range slices.Sorted(maps.Keys(found)) {
		purifiers = append(purifiers, found[serial])
	}

	return purifiers, nil
}

// dial connects to the purifier at --host, or with none to the purifier
// the sticker names, searched for and tried at each place it says it is.
func (root *rootCommand) dial(ctx context.Context) (*dyson.Purifier, error) {
	if root.Host != "" {
		return dyson.Dial(ctx, root.Host, root.identity)
	}

	wanted := func(purifier dyson.Found) bool {
		return purifier.Serial == root.identity.Serial && len(purifier.Addresses) > 0
	}

	purifiers, err := root.search(ctx, wanted)
	if err != nil {
		return nil, err
	}

	failure := fmt.Errorf("%w: the purifier %s did not answer the search within %s; name its address with --host",
		errNotFound, root.identity.Serial, root.Wait)

	for _, purifier := range purifiers {
		if !wanted(purifier) {
			continue
		}

		for _, address := range purifier.Addresses {
			opened, err := dyson.Dial(ctx, address.String(), root.identity)
			if err == nil {
				return opened, nil
			}

			failure = fmt.Errorf("%s: %w", address, err)

			// A credential one address refused is refused at them all,
			// and a purifier's broker counts what it is asked.
			if errors.Is(err, mqtt.ErrCredentials) {
				return nil, failure
			}
		}
	}

	return nil, failure
}

// Run finds the purifiers that announce themselves on this network and
// prints each: who it says it is, and where.
func (*discoverCommand) Run(ctx context.Context, root *rootCommand) error {
	purifiers, err := root.search(ctx, nil)
	if err != nil {
		return err
	}

	if len(purifiers) == 0 {
		terminal.Printf(root.out, "No purifier answered.\n")

		return nil
	}

	table := tabwriter.NewWriter(root.out, 0, 0, 2, ' ', 0)

	terminal.Printf(table, "SERIAL\tTYPE\tAT\n")

	for _, purifier := range purifiers {
		places := make([]string, 0, len(purifier.Addresses))
		for _, address := range purifier.Addresses {
			places = append(places, address.String())
		}

		at := strings.Join(places, ", ")
		if at == "" {
			at = "-"
		}

		terminal.Printf(table, "%s\t%s\t%s\n", purifier.Serial, purifier.Type, at)
	}

	if err = table.Flush(); err != nil {
		return fmt.Errorf("writing the purifiers: %w", err)
	}

	return nil
}

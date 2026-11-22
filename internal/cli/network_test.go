package cli_test

import (
	"bytes"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	mdns "github.com/forkcloser/go-mdns"
	"github.com/forkcloser/go-mdns/mdnstest"
	"github.com/forkcloser/go-mdns/transport"

	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-dyson/internal/cli"
)

// wire is the one wire of the test networks.
const wire = "home"

// announcing puts a purifier on the network, at an address of the
// loopback, announcing itself as a purifier does: its product type and
// its serial for a name, and the port its broker listens on.
//
// Its records are published one by one: Dyson's service type has an
// underscore in it, which RFC 6335 does not allow, and a Peer announces no
// service of such a type.
func announcing(t *testing.T, network *mdnstest.Network, serial, at string) {
	t.Helper()

	address, err := netip.ParseAddrPort(at)
	if err != nil {
		t.Fatalf("the purifier's address %q: %v", at, err)
	}

	host := network.Host(mdnstest.Port{
		Interface: "wlan0", Wire: wire, Addrs: []netip.Prefix{netip.PrefixFrom(address.Addr(), 8)},
	})

	peer, err := mdns.Open(t.Context(), mdns.Config{Network: host})
	if err != nil {
		t.Fatalf("opening the purifier's network: %v", err)
	}

	t.Cleanup(func() { _ = peer.Close() })

	kind := mdns.Name{"_dyson_mqtt", "_tcp", "local"}
	instance := append(mdns.Name{"475_" + serial}, kind...)
	name := mdns.Name{"purifier-" + strings.ToLower(serial), "local"}

	if _, err = peer.Publish(t.Context(), mdns.Records{Records: []mdns.Record{
		{Name: instance, Data: mdns.SRV{Port: address.Port(), Target: name}},
		{Name: instance, Data: mdns.TXT{}},
		{Name: name, Data: mdns.Addr{Addr: address.Addr()}},
	}}); err != nil {
		t.Fatalf("publishing the purifier's records: %v", err)
	}

	if _, err = peer.Publish(t.Context(), mdns.Records{
		Shared: true, Records: []mdns.Record{{Name: kind, Data: mdns.PTR{Target: instance}}},
	}); err != nil {
		t.Fatalf("publishing the purifier's place among its kind: %v", err)
	}
}

// here is the command's own host on the network: on the purifiers' wire
// by en0, and by en1 on one where nothing is.
func here(network *mdnstest.Network) transport.Network {
	return network.Host(
		mdnstest.Port{Interface: "en0", Wire: wire},
		mdnstest.Port{Interface: "en1", Wire: "elsewhere"},
	)
}

// searching is the command with no host given, on a network, under env;
// its output and what it wrote of errors, then its status.
func searching(
	t *testing.T,
	network transport.Network,
	env map[string]string,
	args ...string,
) (out, errs string, status int) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	status = cli.Run(t.Context(), cli.Environment{
		Args: args, Out: &stdout, Err: &stderr, Network: network,
		Getenv: func(name string) string { return env[name] },
	})

	return stdout.String(), stderr.String(), status
}

// TestDiscover: the purifiers that announce themselves are printed, each
// with who it says it is and where, in the order of their serials, with no
// sticker asked for; a network with none says so.
func TestDiscover(t *testing.T) {
	t.Parallel()

	var network mdnstest.Network

	announcing(t, &network, "ZZ9-EU-ABC1234Z", "127.0.0.3:1883")
	announcing(t, &network, dysontest.Serial, "127.0.0.2:1883")

	out, errs, status := searching(t, here(&network), nil, "--wait", "2s", "discover")
	if want := strings.Join([]string{
		"SERIAL TYPE AT",
		dysontest.Serial + " 475 127.0.0.2:1883",
		"ZZ9-EU-ABC1234Z 475 127.0.0.3:1883",
	}, "\n"); status != cli.ExitOK || errs != "" || rows(out) != want {
		t.Errorf("discover exits %d, saying %q and\n%s\nwant\n%s", status, errs, rows(out), want)
	}

	var empty mdnstest.Network

	out, errs, status = searching(t, here(&empty), nil, "--wait", "300ms", "discover")
	if status != cli.ExitOK || errs != "" || out != "No purifier answered.\n" {
		t.Errorf("discover on a network with no purifier exits %d, saying %q and %q", status, errs, out)
	}
}

// TestSearched: with no host given, the purifier the sticker names is
// found on the network and spoken to, whatever else announces itself
// there; one that does not answer is an error that names --host, and so
// is a search on an interface the host has not; a credential the purifier
// refuses is said with where it was found; a wait of nothing is refused.
// The search is on the interface named, and ends once the purifier is
// heard.
func TestSearched(t *testing.T) {
	t.Parallel()

	fake := started(t)

	var network mdnstest.Network

	// Another purifier, at an address where nothing listens.
	announcing(t, &network, "AA1-EU-ABC1234A", "127.0.0.2:"+port(t, fake.Addr()))
	announcing(t, &network, dysontest.Serial, fake.Addr())

	named, _, _ := run(t, fake, sticker(), "status")

	// A wait longer than the command's whole time: the search ends when
	// the purifier is heard, or the command fails.
	out, errs, status := searching(t, here(&network), sticker(), "--wait", "30s", "status")
	if status != cli.ExitOK || errs != "" || out != named {
		t.Errorf("status with the purifier searched for exits %d, saying %q and\n%s\nwant what it prints at --host\n%s",
			status, errs, out, named)
	}

	out, errs, status = searching(t, here(&network), sticker(), "--wait", "30s", "--interface", "en0", "status")
	if status != cli.ExitOK || errs != "" || out != named {
		t.Errorf("status searched for on the purifier's interface exits %d, saying %q and\n%s", status, errs, out)
	}

	wrong := sticker()
	wrong[cli.EnvPassword] = "another-password"

	out, errs, status = searching(t, here(&network), wrong, "--wait", "30s", "status")
	if status != cli.ExitError || out != "" || !strings.Contains(errs, fake.Addr()) ||
		!strings.Contains(errs, "the broker refused the user name or password") ||
		strings.Contains(errs, "another-password") {
		t.Errorf("a credential the purifier found refuses exits %d, saying %q and %q", status, out, errs)
	}

	for _, row := range []struct {
		name string
		args []string
		want string
	}{
		{"the interface the purifier is not on", []string{"--wait", "3s", "--interface", "en1", "status"}, "--host"},
		{"a wait of nothing", []string{"--wait", "0s", "status"}, "--wait wants a time above zero"},
		// The command's own time ending is not a purifier that did not
		// answer: nothing says to name a host.
		{
			"the command's time ending before the wait",
			[]string{"--timeout", "500ms", "--wait", "30s", "--interface", "en1", "status"},
			"searching for purifiers: context deadline exceeded",
		},
	} {
		out, errs, status = searching(t, here(&network), sticker(), row.args...)
		if status != cli.ExitError || out != "" || !strings.Contains(errs, row.want) {
			t.Errorf("%s exits %d, saying %q and %q, want an error with %q", row.name, status, out, errs, row.want)
		}
	}

	var others mdnstest.Network

	announcing(t, &others, "AA1-EU-ABC1234A", fake.Addr())

	before := len(fake.Received())

	out, errs, status = searching(t, here(&others), sticker(), "--wait", "1s", "on")
	if status != cli.ExitError || out != "" || !strings.Contains(errs, dysontest.Serial) ||
		!strings.Contains(errs, "--host") {
		t.Errorf("a purifier that does not answer the search exits %d, saying %q and %q", status, out, errs)
	}

	if sent := fake.Received()[before:]; len(sent) != 0 {
		t.Errorf("another purifier than the sticker's was sent %+v", sent)
	}
}

// port is the port of an address, as it is written.
func port(t *testing.T, address string) string {
	t.Helper()

	_, written, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("the address %q: %v", address, err)
	}

	if _, err = strconv.Atoi(written); err != nil {
		t.Fatalf("the port of %q: %v", address, err)
	}

	return written
}

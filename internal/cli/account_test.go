package cli_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/account"
	"github.com/duncanite/go-dyson/dyson/account/accounttest"
	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-dyson/internal/cli"
)

// owner is the fake account, as the environment holds it.
func owner() map[string]string {
	return map[string]string{
		cli.EnvAccountEmail:    accounttest.Email,
		cli.EnvAccountPassword: accounttest.Password,
		cli.EnvAccountCountry:  accounttest.Country,
	}
}

// asking is the account command against the fake API, under env, with
// typed on its input; its output and what it wrote of errors, then its
// status.
func asking(
	t *testing.T, api *accounttest.API, env map[string]string, typed string, flags ...string,
) (out, errs string, status int) {
	t.Helper()

	return reading(t, api, env, strings.NewReader(typed), flags...)
}

// reading is asking, with what is typed read from anywhere.
func reading(
	t *testing.T, api *accounttest.API, env map[string]string, typed io.Reader, flags ...string,
) (out, errs string, status int) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	status = cli.Run(t.Context(), cli.Environment{
		Args: append(flags, "account"), In: typed, Out: &stdout, Err: &stderr,
		Getenv:  func(name string) string { return env[name] },
		Account: account.Options{BaseURL: api.URL, HTTP: api.Client()},
	})

	return stdout.String(), stderr.String(), status
}

// TestAccount: the account command asks Dyson for a code, takes the one
// typed, and prints each of the account's purifiers under the names of the
// variables that take it; a name is shown and never obeyed. It asks for no
// purifier's identity. What it cannot do is said, with no secret in it,
// and nothing is printed.
func TestAccount(t *testing.T) {
	t.Parallel()

	api := accounttest.New()
	t.Cleanup(api.Close)

	api.Devices = []accounttest.Device{
		{Serial: "AB1-EU-CDE2345F", Name: "Bedroom", Product: "475", Credential: "first-credential"},
		{
			Serial:     "AB3-EU-CDE2345F",
			Name:       "Den\ndyson: all is well\u202e",
			Product:    "527",
			Credential: "second-credential",
		},
	}

	out, errs, status := asking(t, api, owner(), accounttest.Code+"\n")
	if want := strings.Join([]string{
		"NAME DYSON_SERIAL DYSON_TYPE DYSON_CREDENTIAL",
		"Bedroom AB1-EU-CDE2345F 475 first-credential",
		`Den\ndyson: all is well\u202e AB3-EU-CDE2345F 527 second-credential`,
	}, "\n"); status != cli.ExitOK || rows(out) != want || !strings.Contains(errs, "The code: ") ||
		!strings.Contains(errs, "keep it as a password is kept") {
		t.Errorf("account exits %d, saying %q and\n%s\nwant\n%s", status, errs, rows(out), want)
	}

	if _, codes := api.Received(); codes != 1 {
		t.Errorf("one run of account had %d codes sent", codes)
	}

	api.Devices = nil

	out, _, status = asking(t, api, owner(), "  "+accounttest.Code+"  \r\n")
	if status != cli.ExitOK || out != "The account has no device to speak to on the network.\n" {
		t.Errorf("account with no device, the code typed with spaces around it, exits %d saying %q", status, out)
	}

	wrong := owner()
	wrong[cli.EnvAccountPassword] = "another-password"

	for _, row := range []struct {
		name  string
		env   map[string]string
		typed string
		want  string
	}{
		{"a code Dyson refuses", owner(), "000000\n", "dyson refused the password, the code or the session"},
		{"a password Dyson refuses", wrong, accounttest.Code + "\n", "dyson refused the password, the code or the session"},
		{"no code typed", owner(), "\n", "no code was typed"},
		{"nothing typed at all", owner(), "", "no code was typed"},
		{
			"an account that is not set",
			map[string]string{cli.EnvAccountEmail: accounttest.Email},
			accounttest.Code + "\n",
			"not set in the environment: " + cli.EnvAccountPassword + ", " + cli.EnvAccountCountry,
		},
	} {
		out, errs, status = asking(t, api, row.env, row.typed)
		if status != cli.ExitError || out != "" || !strings.Contains(errs, row.want) {
			t.Errorf("%s exits %d, saying %q and %q, want an error with %q", row.name, status, out, errs, row.want)
		}

		for _, secret := range []string{accounttest.Password, "another-password", accounttest.Email, "session-"} {
			if strings.Contains(errs, secret) {
				t.Errorf("%s shows %q: %q", row.name, secret, errs)
			}
		}
	}
}

// TestAccountBounds: what is typed where the code goes is read no further
// than a code may be, and nothing of it is sent; what is asked of Dyson is
// bounded by --timeout, before the code and after it; a device that could
// not be read is said after the others, which are printed all the same.
func TestAccountBounds(t *testing.T) {
	t.Parallel()

	api := accounttest.New()
	t.Cleanup(api.Close)

	for name, typed := range map[string]string{
		"a line longer than a code":   strings.Repeat("9", 300) + "\n",
		"an input that never ends it": strings.Repeat("9", 1<<20),
	} {
		before, _ := api.Received()
		input := &counted{from: strings.NewReader(typed)}

		out, errs, status := reading(t, api, owner(), input)
		if status != cli.ExitError || out != "" || !strings.Contains(errs, "more than 256 characters typed") {
			t.Errorf("%s exits %d, saying %q and %q", name, status, out, errs)
		}

		if input.read > 257 {
			t.Errorf("%s: %d bytes were read of it, where a code is 256 at most", name, input.read)
		}

		after, _ := api.Received()
		for _, request := range after[len(before):] {
			if strings.HasSuffix(request.Path, "/verify") {
				t.Errorf("%s was sent to Dyson: %d bytes", name, len(request.Body))
			}
		}
	}

	api.Devices = []accounttest.Device{
		{Serial: "AB1-EU-CDE2345F", Name: "Bedroom", Product: "475", Credential: "first-credential"},
		{Serial: "AB2-EU-CDE2345F", Name: "Unreadable", Product: "475", Sealed: "not base64!"},
	}

	out, errs, status := asking(t, api, owner(), accounttest.Code+"\n")
	if status != cli.ExitError || !strings.Contains(rows(out), "Bedroom AB1-EU-CDE2345F 475 first-credential") ||
		strings.Contains(out, "Unreadable") ||
		!strings.Contains(errs, "a device of the account could not be read: 1 of the 2") {
		t.Errorf("an account with a device that does not read exits %d, saying %q and\n%s", status, errs, out)
	}

	if out, errs, status = asking(t, api, owner(), accounttest.Code+"\n", "--timeout", "0s"); status != cli.ExitError ||
		out != "" || !strings.Contains(errs, "--timeout wants a time above zero") {
		t.Errorf("account with no time exits %d, saying %q and %q", status, out, errs)
	}

	// An API that takes the request and never answers it.
	api.Stall = true

	began := time.Now()

	out, errs, status = asking(t, api, owner(), accounttest.Code+"\n", "--timeout", "300ms")
	if took := time.Since(began); status != cli.ExitError || out != "" ||
		!strings.Contains(errs, "context deadline exceeded") || took > 10*time.Second {
		t.Errorf(
			"account against an API that never answers exits %d after %s, saying %q and %q",
			status,
			took,
			out,
			errs,
		)
	}
}

// counted is an input that says how much of it was read.
type counted struct {
	from io.Reader
	read int
}

func (c *counted) Read(into []byte) (int, error) {
	got, err := c.from.Read(into)
	c.read += got

	return got, err
}

// TestNamedWithNoSticker: a purifier named by the three values the account
// gives is spoken to as one named by its sticker; one or two of the three
// are refused, by the names of those missing, and nothing is sent.
func TestNamedWithNoSticker(t *testing.T) {
	t.Parallel()

	fake := started(t)

	identity, err := dyson.FromSticker(dysontest.SSID, dysontest.WiFiPassword)
	if err != nil {
		t.Fatal(err)
	}

	named := map[string]string{
		cli.EnvSerial: identity.Serial, cli.EnvType: identity.Type, cli.EnvCredential: identity.Credential,
	}

	stuck, _, _ := run(t, fake, sticker(), "status")

	out, errs, status := run(t, fake, named, "status")
	if status != cli.ExitOK || errs != "" || out != stuck {
		t.Errorf(
			"status of a purifier named with no sticker exits %d, saying %q and\n%s\nwant\n%s",
			status,
			errs,
			out,
			stuck,
		)
	}

	before := len(fake.Received())

	// The sticker beside them changes nothing: the three were given, and
	// are not whole.
	partial := sticker()
	partial[cli.EnvSerial] = identity.Serial

	out, errs, status = run(t, fake, partial, "on")
	if want := "with the others of the three: " + cli.EnvCredential + ", " + cli.EnvType; status != cli.ExitError ||
		out != "" || !strings.Contains(errs, want) {
		t.Errorf("one of the three alone exits %d, saying %q and %q, want an error with %q", status, out, errs, want)
	}

	if strings.Contains(errs, identity.Serial) {
		t.Errorf("the refusal shows the serial: %q", errs)
	}

	if sent := fake.Received()[before:]; len(sent) != 0 {
		t.Errorf("a purifier half named was sent %+v", sent)
	}
}

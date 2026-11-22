package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/duncanite/go-dyson/dyson/account"
	"github.com/duncanite/go-dyson/internal/terminal"
)

// maxCode bounds what is read where the code is typed: more than that is
// no code, and is not read on.
const maxCode = 256

// Run logs in to the Dyson account the environment names, with the code
// Dyson then sends by mail, typed where it is asked for, and prints the
// identity of each of the account's purifiers: what the environment takes
// for a purifier with no sticker. What it prints opens the purifiers.
//
// --timeout bounds what is asked of Dyson, before the code and after it;
// the typing of the code takes as long as the person does.
func (*accountCommand) Run(ctx context.Context, root *rootCommand) error {
	client, err := root.dyson()
	if err != nil {
		return err
	}

	asking, done := context.WithTimeout(ctx, root.Timeout)

	challenge, err := client.Ask(asking, root.getenv(EnvAccountEmail))

	done()

	if err != nil {
		return err
	}

	terminal.Printf(root.err, "Dyson has sent a code to the account's address. The code: ")

	code, err := typed(root.in)
	if err != nil {
		return err
	}

	fetching, done := context.WithTimeout(ctx, root.Timeout)
	defer done()

	session, err := client.Answer(fetching, challenge, root.getenv(EnvAccountPassword), code)
	if err != nil {
		return err
	}

	// A device that could not be read is said after the others, which are
	// printed all the same.
	devices, unread := client.Devices(fetching, session)
	if unread != nil && !errors.Is(unread, account.ErrUnread) {
		return unread
	}

	if err = root.identities(devices, unread == nil); err != nil {
		return err
	}

	return unread
}

// dyson is the account's API for the account the environment names; what
// the command cannot run with is said.
func (root *rootCommand) dyson() (*account.Client, error) {
	var missing []string

	for _, name := range []string{EnvAccountEmail, EnvAccountPassword, EnvAccountCountry} {
		if root.getenv(name) == "" {
			missing = append(missing, name)
		}
	}

	switch {
	case len(missing) > 0:
		return nil, fmt.Errorf("%w: not set in the environment: %s", errUsage, strings.Join(missing, ", "))
	case root.in == nil:
		return nil, fmt.Errorf("%w: nowhere to read the code from", errUsage)
	case root.Timeout <= 0:
		return nil, fmt.Errorf("%w: --timeout wants a time above zero", errUsage)
	}

	opts := root.api
	opts.Country = root.getenv(EnvAccountCountry)

	return account.New(opts), nil
}

// identities prints the devices, each under the names of the variables
// that take it. That there are none is said only when the list was whole.
func (root *rootCommand) identities(devices []account.Device, whole bool) error {
	if len(devices) == 0 {
		if whole {
			terminal.Printf(root.out, "The account has no device to speak to on the network.\n")
		}

		return nil
	}

	table := tabwriter.NewWriter(root.out, 0, 0, 2, ' ', 0)

	terminal.Printf(table, "NAME\t%s\t%s\t%s\n", EnvSerial, EnvType, EnvCredential)

	for _, device := range devices {
		terminal.Printf(table, "%s\t%s\t%s\t%s\n",
			device.Name, device.Identity.Serial, device.Identity.Type, device.Identity.Credential)
	}

	if err := table.Flush(); err != nil {
		return fmt.Errorf("writing the devices: %w", err)
	}

	terminal.Printf(root.err, "A credential opens its purifier: keep it as a password is kept.\n")

	return nil
}

// typed is the code read from where it is typed: one line, without the
// space around it. An empty line is no code, and neither is more than a
// code may be.
func typed(from io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(from, maxCode+1)).ReadString('\n')

	if len(line) > maxCode {
		return "", fmt.Errorf("%w: more than %d characters typed where the code goes", errUsage, maxCode)
	}

	code := strings.TrimSpace(line)
	if code == "" {
		if err == nil || errors.Is(err, io.EOF) {
			err = errUsage
		}

		return "", fmt.Errorf("no code was typed: %w", err)
	}

	return code, nil
}

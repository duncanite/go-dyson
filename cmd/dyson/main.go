// dyson reads and drives a Dyson Pure Cool Link purifier on the LAN; the
// command is internal/cli.
package main

import (
	"context"
	"os"

	"github.com/mycophonic/primordium/app"
	"github.com/mycophonic/primordium/app/shutdown"

	"github.com/duncanite/go-dyson/internal/cli"
)

func main() {
	app.Main(&app.Options{Name: "dyson"}, func(ctx context.Context) error {
		env := cli.Environment{Args: os.Args[1:], In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv}

		if code := cli.Run(ctx, env); code != cli.ExitOK {
			return shutdown.ExitError(code)
		}

		return nil
	})
}

// Package cli is the dyson command: what a Pure Cool Link purifier on the
// LAN says of itself and of the air, and the commands that drive its fan.
// The purifier's sticker, its network name and its Wi-Fi password, comes
// from the environment and from nowhere else. Its address is given, or
// the purifier the sticker names is searched for on the network.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"
	"github.com/forkcloser/go-mdns/transport"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/internal/terminal"
)

// Exit statuses.
const (
	ExitOK    = 0
	ExitError = 1
)

// The environment variables the sticker is read from.
const (
	EnvSSID     = "DYSON_SSID"
	EnvPassword = "DYSON_WIFI_PASSWORD" // #nosec G101 -- the name of an environment variable, not a credential
)

// errUsage is a command line, or an environment, the command cannot act
// on.
var errUsage = errors.New("cannot run")

// exitStatus is how the parser's own exit, on --help, leaves Run: kong
// calls its exit function and goes on, so the function panics with the
// status and Run recovers it.
type exitStatus int

// Environment is what the command runs in.
type Environment struct {
	// Args is its arguments, without its name.
	Args []string
	// Out and Err are where it writes.
	Out io.Writer
	Err io.Writer
	// Getenv reads the environment, where the purifier's sticker is.
	Getenv func(string) string
	// Network is where purifiers are searched for; nil is the host's own.
	Network transport.Network
}

type rootCommand struct {
	Host      string        `env:"DYSON_HOST"      help:"The purifier's address on the network; with none, the purifier the sticker names is searched for."`
	Interface string        `env:"DYSON_INTERFACE" help:"The network interface the purifiers are on, when the host has several."`
	Wait      time.Duration `default:"3s"          env:"DYSON_WAIT"                                                                                         help:"How long a search waits for purifiers to answer."`
	Timeout   time.Duration `default:"15s"         help:"How long to wait for the purifier, its search included, for every command but watch."`

	Discover  discoverCommand  `cmd:"" help:"Print the purifiers that announce themselves on this network. Needs no sticker."`
	Status    statusCommand    `cmd:"" help:"Print how the purifier is set and what its sensors read."`
	On        onCommand        `cmd:"" help:"Turn the fan on, at the speed it had."`
	Off       offCommand       `cmd:"" help:"Turn the fan off."`
	Auto      autoCommand      `cmd:"" help:"Let the purifier run the fan as the air needs."`
	Speed     speedCommand     `cmd:"" help:"Turn the fan on at a speed."`
	Oscillate oscillateCommand `cmd:"" help:"Have the purifier turn from side to side, or stop."`
	Night     nightCommand     `cmd:"" help:"Set the quiet, dimmed night mode, or end it."`
	Timer     timerCommand     `cmd:"" help:"Have the purifier turn itself off after a time."`
	Watch     watchCommand     `cmd:"" help:"Print what the purifier says as it says it, until interrupted."`

	out      io.Writer
	network  transport.Network
	identity dyson.Identity
}

type toggle struct {
	State string `arg:"" enum:"on,off" help:"on or off."`
}

type (
	discoverCommand  struct{}
	statusCommand    struct{}
	onCommand        struct{}
	offCommand       struct{}
	autoCommand      struct{}
	watchCommand     struct{}
	oscillateCommand struct{ toggle }
	nightCommand     struct{ toggle }
	speedCommand     struct {
		Speed int `arg:"" help:"The speed, 1 to 10."`
	}
	timerCommand struct {
		After string `arg:"" help:"How long from now, in hours and minutes: 90m, 2h. Nine hours at most. off ends the timer."`
	}
)

// Run parses the arguments and runs the command.
func Run(ctx context.Context, env Environment) (status int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exited, ok := recovered.(exitStatus)
			if !ok {
				panic(recovered)
			}

			status = int(exited)
		}
	}()

	root := rootCommand{out: env.Out, network: env.Network}

	parser, err := kong.New(&root,
		kong.Name("dyson"),
		kong.Description("Read and drive a Dyson Pure Cool Link purifier on the LAN. Its sticker is read from "+
			EnvSSID+", the network name printed on it, and "+EnvPassword+", the password beside it."),
		kong.UsageOnError(),
		kong.Writers(env.Out, env.Err),
		kong.Exit(func(code int) { panic(exitStatus(code)) }),
		kong.BindTo(ctx, (*context.Context)(nil)),
	)
	if err != nil {
		terminal.Printf(env.Err, "dyson: %s\n", err)

		return ExitError
	}

	kctx, err := parser.Parse(env.Args)
	if err != nil {
		parser.Errorf("%s", err)

		return ExitError
	}

	// What is on the network is anyone's to hear: discover asks for no
	// sticker.
	if kctx.Command() != "discover" {
		err = root.identify(env.Getenv)
	}

	if err == nil {
		err = kctx.Run(&root)
	}

	if err != nil {
		terminal.Printf(env.Err, "dyson: %s\n", err)

		return ExitError
	}

	return ExitOK
}

// identify reads the sticker from the environment; what is not set is
// said by its variable's name.
func (root *rootCommand) identify(getenv func(string) string) error {
	var missing []string

	for _, name := range []string{EnvSSID, EnvPassword} {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("%w: not set in the environment: %s", errUsage, strings.Join(missing, ", "))
	}

	identity, err := dyson.FromSticker(getenv(EnvSSID), getenv(EnvPassword))
	if err != nil {
		return fmt.Errorf("%s: %w", EnvSSID, err)
	}

	root.identity = identity

	return nil
}

// with connects to the purifier, runs do within the timeout, and closes
// the connection: a purifier's broker counts them.
func (root *rootCommand) with(ctx context.Context, action func(context.Context, *dyson.Purifier) error) error {
	if root.Timeout <= 0 {
		return fmt.Errorf("%w: --timeout wants a time above zero", errUsage)
	}

	ctx, cancel := context.WithTimeout(ctx, root.Timeout)
	defer cancel()

	return root.connected(ctx, action)
}

func (root *rootCommand) connected(ctx context.Context, action func(context.Context, *dyson.Purifier) error) error {
	purifier, err := root.dial(ctx)
	if err != nil {
		return err
	}

	defer func() { _ = purifier.Close() }()

	return action(ctx, purifier)
}

func (*statusCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		state, err := purifier.State(ctx)
		if err != nil {
			return err
		}

		environment, err := purifier.Environment(ctx)
		if err != nil {
			return err
		}

		table := tabwriter.NewWriter(root.out, 0, 0, 1, ' ', 0)

		for _, line := range append(settings(&state), readings(&environment)...) {
			terminal.Printf(table, "%s:\t%s\n", line[0], line[1])
		}

		if err = table.Flush(); err != nil {
			return fmt.Errorf("writing the status: %w", err)
		}

		return nil
	})
}

// settings is a state as lines of a name and a value.
func settings(state *dyson.State) [][2]string {
	mode := map[dyson.Mode]string{dyson.Off: "off", dyson.Fan: "on", dyson.Auto: "auto"}[state.Mode]
	if state.Speed > 0 {
		mode += fmt.Sprintf(", speed %d", state.Speed)
	}

	lines := [][2]string{
		{"fan", mode + ", " + map[bool]string{true: "turning", false: "still"}[state.Running]},
		{"oscillating", yesNo(state.Oscillating)},
		{"night mode", yesNo(state.NightMode)},
		{"monitoring", yesNo(state.Monitoring)},
		{"filter", fmt.Sprintf("%d hours left", int(state.FilterLife/time.Hour))},
	}

	// The codes are the purifier's own text.
	if state.Error != "" {
		lines = append(lines, [2]string{"error", state.Error})
	}

	if state.Warning != "" {
		lines = append(lines, [2]string{"warning", state.Warning})
	}

	return lines
}

// readings is the sensors as lines of a name and a value, a dash for a
// sensor that reads nothing.
func readings(environment *dyson.Environment) [][2]string {
	temperature := "-"

	if environment.Temperature != nil {
		celsius := *environment.Temperature
		temperature = fmt.Sprintf("%.1fC, %.1fF", celsius, celsius*9/5+32)
	}

	lines := [][2]string{
		{"temperature", temperature},
		{"humidity", number(environment.Humidity, "%")},
		{"particulates", number(environment.Particulates, "")},
		{"volatile compounds", number(environment.VOC, "")},
	}

	if environment.SleepTimer > 0 {
		lines = append(lines, [2]string{"sleep timer", environment.SleepTimer.String() + " left"})
	}

	return lines
}

func number(value *int, unit string) string {
	if value == nil {
		return "-"
	}

	return fmt.Sprintf("%d%s", *value, unit)
}

func yesNo(yes bool) string {
	if yes {
		return "yes"
	}

	return "no"
}

func (*onCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetMode(ctx, dyson.Fan)
	})
}

func (*offCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetMode(ctx, dyson.Off)
	})
}

func (*autoCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetMode(ctx, dyson.Auto)
	})
}

func (cmd *speedCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetSpeed(ctx, cmd.Speed)
	})
}

func (cmd *oscillateCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetOscillation(ctx, cmd.State == "on")
	})
}

func (cmd *nightCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetNightMode(ctx, cmd.State == "on")
	})
}

func (cmd *timerCommand) Run(ctx context.Context, root *rootCommand) error {
	var after time.Duration

	if cmd.After != "off" {
		asked, err := time.ParseDuration(cmd.After)
		if err != nil || asked <= 0 {
			return fmt.Errorf("%w: %q is neither a time as 90m or 2h, nor off", errUsage, cmd.After)
		}

		after = asked
	}

	return root.with(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		return purifier.SetSleepTimer(ctx, after)
	})
}

// Run prints what the purifier says, one line each, until the context is
// done, which is how the command is told to stop.
func (*watchCommand) Run(ctx context.Context, root *rootCommand) error {
	return root.connected(ctx, func(ctx context.Context, purifier *dyson.Purifier) error {
		// Ask once, so that there is something to see before anything
		// changes.
		state, err := purifier.State(ctx)
		update := dyson.Update{State: &state}

		for err == nil {
			terminal.Printf(root.out, "%s  %s\n", time.Now().Format(time.TimeOnly), line(update))

			update, err = purifier.Next(ctx)
		}

		if ctx.Err() != nil {
			return nil //nolint:nilerr // told to stop: the context's end is how watch ends, and no failure
		}

		return err
	})
}

// line is what the purifier said, in one line.
func line(update dyson.Update) string {
	var fields [][2]string

	if update.State != nil {
		fields = settings(update.State)
	}

	if update.Environment != nil {
		fields = readings(update.Environment)
	}

	said := make([]string, 0, len(fields))
	for _, field := range fields {
		said = append(said, field[0]+" "+field[1])
	}

	return strings.Join(said, "; ")
}

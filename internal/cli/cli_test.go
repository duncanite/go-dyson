package cli_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-dyson/internal/cli"
)

func started(t *testing.T) *dysontest.Purifier {
	t.Helper()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	return fake
}

// sticker is the fake's sticker, as the environment holds it.
func sticker() map[string]string {
	return map[string]string{cli.EnvSSID: dysontest.SSID, cli.EnvPassword: dysontest.WiFiPassword}
}

// run is the command against the fake, under env; its output and what it
// wrote of errors, then its status.
func run(t *testing.T, fake *dysontest.Purifier, env map[string]string, args ...string) (out, errs string, status int) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	args = append([]string{"--host", fake.Addr()}, args...)
	status = cli.Run(t.Context(), cli.Environment{
		Args: args, Out: &stdout, Err: &stderr, Getenv: func(name string) string { return env[name] },
	})

	return stdout.String(), stderr.String(), status
}

// rows is the output as lines of fields, the spacing of a table aside.
func rows(out string) string {
	var lines []string

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}

	return strings.Join(lines, "\n")
}

// TestStatus: how the purifier is set and what its sensors read, as a
// person reads it; a sensor that reads nothing is a dash, and a code or a
// timer shows when there is one.
func TestStatus(t *testing.T) {
	t.Parallel()

	fake := started(t)

	out, errs, status := run(t, fake, sticker(), "status")
	if want := strings.Join([]string{
		"fan: on, speed 4, turning",
		"oscillating: yes",
		"night mode: no",
		"monitoring: yes",
		"filter: 2159 hours left",
		"temperature: 24.6C, 76.2F",
		"humidity: 58%",
		"particulates: 0",
		"volatile compounds: 3",
	}, "\n"); status != cli.ExitOK || errs != "" || rows(out) != want {
		t.Errorf("status exits %d, saying %q and\n%s\nwant\n%s", status, errs, rows(out), want)
	}

	fake.Product["fmod"], fake.Product["fnst"], fake.Product["fnsp"] = "AUTO", "OFF", "AUTO"
	fake.Product["ercd"], fake.Product["wacd"] = "02C0", "FLTR"
	fake.Sensors["tact"], fake.Sensors["hact"], fake.Sensors["sltm"] = "OFF", "INIT", "0090"

	out, errs, status = run(t, fake, sticker(), "status")
	if want := strings.Join([]string{
		"fan: auto, still",
		"oscillating: yes",
		"night mode: no",
		"monitoring: yes",
		"filter: 2159 hours left",
		"error: 02C0",
		"warning: FLTR",
		"temperature: -",
		"humidity: -",
		"particulates: 0",
		"volatile compounds: 3",
		"sleep timer: 1h30m0s left",
	}, "\n"); status != cli.ExitOK || errs != "" || rows(out) != want {
		t.Errorf("status in auto exits %d, saying %q and\n%s\nwant\n%s", status, errs, rows(out), want)
	}
}

// TestActing: each command that changes something sends its one change,
// prints nothing, and leaves the purifier with no connection held.
func TestActing(t *testing.T) {
	t.Parallel()

	fake := started(t)

	for _, step := range []struct {
		args []string
		want map[string]string
	}{
		{[]string{"off"}, map[string]string{"fmod": "OFF"}},
		{[]string{"on"}, map[string]string{"fmod": "FAN"}},
		{[]string{"auto"}, map[string]string{"fmod": "AUTO"}},
		{[]string{"speed", "7"}, map[string]string{"fmod": "FAN", "fnsp": "0007"}},
		{[]string{"oscillate", "off"}, map[string]string{"oson": "OFF"}},
		{[]string{"oscillate", "on"}, map[string]string{"oson": "ON"}},
		{[]string{"night", "on"}, map[string]string{"nmod": "ON"}},
		{[]string{"timer", "2h"}, map[string]string{"sltm": "0120"}},
		{[]string{"timer", "off"}, map[string]string{"sltm": "OFF"}},
	} {
		before := len(fake.Received())

		out, errs, status := run(t, fake, sticker(), step.args...)
		if status != cli.ExitOK || out != "" || errs != "" {
			t.Fatalf("%v exits %d, saying %q and %q", step.args, status, out, errs)
		}

		sent := fake.Received()[before:]
		if len(sent) != 1 || sent[0].Message != "STATE-SET" || !reflect.DeepEqual(sent[0].Data, step.want) {
			t.Errorf("%v sent %+v, want a STATE-SET of %v", step.args, sent, step.want)
		}
	}

	// Each run closed its connection: the purifier's broker forgets the
	// subscription when it does.
	deadline := time.Now().Add(5 * time.Second)
	for len(fake.Filters()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if held := fake.Filters(); len(held) != 0 {
		t.Errorf("after the commands the purifier still holds %v", held)
	}
}

// TestAPurifiersWords: what a purifier says is shown and never obeyed: a
// code with a line's end, an escape sequence and a direction mark in it is
// written out, in the status and under watch, and forges no line of the
// command's.
func TestAPurifiersWords(t *testing.T) {
	t.Parallel()

	fake := started(t)
	fake.Product["ercd"] = "02C0\ndyson: all is well\u202e\x1b[2J"

	const shown = `02C0\ndyson: all is well\u202e\x1b[2J`

	plain, _, _ := run(t, started(t), sticker(), "status")

	said, errs, status := run(t, fake, sticker(), "status")
	if status != cli.ExitOK || errs != "" || !strings.Contains(said, "error:") || !strings.Contains(said, shown) ||
		strings.Count(said, "\n") != strings.Count(plain, "\n")+1 {
		t.Errorf("status of a purifier with a hostile code exits %d, saying %q and\n%s", status, errs, said)
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	var (
		stdout out
		stderr bytes.Buffer
		ended  = make(chan int, 1)
	)

	env := sticker()

	go func() {
		ended <- cli.Run(ctx, cli.Environment{
			Args: []string{"--host", fake.Addr(), "watch"}, Out: &stdout, Err: &stderr,
			Getenv: func(name string) string { return env[name] },
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(stdout.lines()) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	stop()

	if exit := <-ended; exit != cli.ExitOK {
		t.Errorf("watch, told to stop, exits %d saying %q", exit, stderr.String())
	}

	if lines := stdout.lines(); len(lines) != 1 || !strings.Contains(lines[0], "error "+shown) {
		t.Errorf("watch of a purifier with a hostile code printed %q", lines)
	}
}

// TestRefusing: what the command cannot do, it says on the error stream
// and exits 1, with no change sent: a sticker that is not set or is none,
// one the purifier refuses, a value the purifier has no meaning for. No
// error shows the password. --help exits 0 and names the variables.
func TestRefusing(t *testing.T) {
	t.Parallel()

	fake := started(t)

	wrong := sticker()
	wrong[cli.EnvPassword] = "another-password"

	renamed := sticker()
	renamed[cli.EnvSSID] = "MyNetwork"

	for _, step := range []struct {
		env  map[string]string
		args []string
		want string
	}{
		{map[string]string{}, []string{"status"}, "not set in the environment: " + cli.EnvSSID + ", " + cli.EnvPassword},
		{renamed, []string{"status"}, "not a Dyson sticker's network name: it reads DYSON-<serial>-<type>"},
		{wrong, []string{"on"}, "the broker refused the user name or password"},
		{sticker(), []string{"speed", "11"}, "a speed of 11, outside 1 to 10"},
		{sticker(), []string{"speed", "fast"}, "fast"},
		{sticker(), []string{"oscillate", "maybe"}, "maybe"},
		{sticker(), []string{"timer", "10h"}, "a sleep timer of 10h0m0s"},
		{sticker(), []string{"timer", "soon"}, `"soon" is neither a time as 90m or 2h, nor off`},
		{sticker(), []string{"--timeout", "0s", "on"}, "--timeout wants a time above zero"},
	} {
		out, errs, status := run(t, fake, step.env, step.args...)
		if status != cli.ExitError || out != "" || !strings.Contains(errs, step.want) {
			t.Errorf("%v exits %d, saying %q and %q, want an error with %q", step.args, status, out, errs, step.want)
		}

		if strings.Contains(errs, "another-password") || strings.Contains(errs, dysontest.WiFiPassword) {
			t.Errorf("%v shows the password: %q", step.args, errs)
		}
	}

	if sent := fake.Received(); len(sent) != 0 {
		t.Errorf("the refused commands sent %+v", sent)
	}

	help, _, status := run(t, fake, nil, "--help")
	if status != cli.ExitOK || !strings.Contains(help, cli.EnvSSID) || !strings.Contains(help, cli.EnvPassword) {
		t.Errorf("--help exits %d saying %q", status, help)
	}
}

// out is where watch writes, read while it runs.
type out struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (o *out) Write(written []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.text.Write(written)
}

func (o *out) lines() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	written := strings.TrimSpace(o.text.String())
	if written == "" {
		return nil
	}

	return strings.Split(written, "\n")
}

// TestWatch: watch prints the state at once, then each thing the purifier
// says as it says it, and ends, with no error, when it is told to stop.
func TestWatch(t *testing.T) {
	t.Parallel()

	fake := started(t)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	var (
		stdout out
		stderr bytes.Buffer
		ended  = make(chan int, 1)
	)

	env := sticker()

	go func() {
		ended <- cli.Run(ctx, cli.Environment{
			Args: []string{"--host", fake.Addr(), "watch"}, Out: &stdout, Err: &stderr,
			Getenv: func(name string) string { return env[name] },
		})
	}()

	// until waits for watch to have printed that many lines.
	until := func(count int) []string {
		t.Helper()

		deadline := time.Now().Add(5 * time.Second)
		for len(stdout.lines()) < count && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}

		lines := stdout.lines()
		if len(lines) < count {
			t.Fatalf("watch printed %d lines, want %d: %q; errors %q", len(lines), count, lines, stderr.String())
		}

		return lines
	}

	if first := until(1)[0]; !strings.Contains(first, "fan on, speed 4, turning; oscillating yes") {
		t.Errorf("watch began with %q", first)
	}

	fake.Set("oson", "OFF")

	if second := until(2)[1]; !strings.Contains(second, "oscillating no") {
		t.Errorf("after a change at the purifier, watch printed %q", second)
	}

	fake.Say("ENVIRONMENTAL-CURRENT-SENSOR-DATA", "data", map[string]string{"hact": "0041", "tact": "2950"})

	if third := until(3)[2]; !strings.Contains(third, "temperature 21.9C, 71.3F; humidity 41%") {
		t.Errorf("after the sensors spoke, watch printed %q", third)
	}

	stop()

	select {
	case status := <-ended:
		if status != cli.ExitOK || stderr.Len() != 0 {
			t.Errorf("watch, told to stop, exits %d saying %q", status, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not end when told to stop")
	}
}

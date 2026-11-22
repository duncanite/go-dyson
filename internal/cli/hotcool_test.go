package cli_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-dyson/internal/cli"
)

// warm is the fake Hot+Cool's sticker, as the environment holds it.
func warm() map[string]string {
	return map[string]string{cli.EnvSSID: dysontest.HotCoolSSID, cli.EnvPassword: dysontest.HotCoolWiFiPassword}
}

// TestHotCoolStatus: a Hot+Cool's status is its own: its two filters, its
// heater and what it heats to, the particulates by their size and the two
// indices; a reading it has none of is a dash.
func TestHotCoolStatus(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	out, errs, status := run(t, fake, warm(), "status")
	if want := strings.Join([]string{
		"fan: on, speed 5, turning",
		"oscillating: yes",
		"night mode: no",
		"monitoring: yes",
		"filters: HEPA 95% left, carbon 80% left",
		"heat: off, to 20.9C, 69.5F",
		"temperature: 21.9C, 71.3F",
		"humidity: 45%",
		"PM2.5: 7 ug/m3",
		"PM10: 10 ug/m3",
		"volatile compounds: 0.4",
		"nitrogen dioxide: 1.2",
	}, "\n"); status != cli.ExitOK || errs != "" || rows(out) != want {
		t.Errorf("status of a Hot+Cool exits %d, saying %q and\n%s\nwant\n%s", status, errs, rows(out), want)
	}

	fake.Product["hmod"], fake.Product["hsta"], fake.Product["cflr"] = "HEAT", "HEAT", "INV"
	fake.Sensors["va10"], fake.Sensors["p25r"], fake.Sensors["pm25"] = "INIT", "OFF", "OFF"

	out, errs, status = run(t, fake, warm(), "status")
	if status != cli.ExitOK || errs != "" ||
		!strings.Contains(rows(out), "filters: HEPA 95% left, carbon - left\nheat: on, heating, to 20.9C, 69.5F\n") ||
		!strings.Contains(rows(out), "PM2.5: -\nPM10: 10 ug/m3\nvolatile compounds: -\n") {
		t.Errorf("status of a Hot+Cool that heats exits %d, saying %q and\n%s", status, errs, rows(out))
	}

	fake.Product["hsta"] = "OFF"

	if out, _, _ = run(t, fake, warm(), "status"); !strings.Contains(rows(out), "heat: on, not heating, to 20.9C") {
		t.Errorf("status of a Hot+Cool at its target says\n%s", rows(out))
	}

	// A target is held to the tenth of a kelvin: the nearest it can hold to
	// a whole degree, of either scale, is printed as that degree, and no
	// other value is moved.
	for held, want := range map[string]string{
		"2942": "to 21.0C, 69.9F", // set to 21 degrees Celsius
		"2859": "to 12.8C, 55.0F", // set to 55 degrees Fahrenheit
		"2932": "to 20.0C, 68.0F", // set to 20 degrees Celsius, which is 68 Fahrenheit
		"2943": "to 21.2C, 70.0F", // set to 70 degrees Fahrenheit
		"2941": "to 21.0C, 69.7F", // as near 21 degrees Celsius as 2942 is
		"2945": "to 21.4C, 70.4F", // near no whole degree
	} {
		fake.Product["hmax"] = held

		if out, _, _ = run(t, fake, warm(), "status"); !strings.Contains(rows(out), want+"\n") {
			t.Errorf("a target held as %s is printed in\n%s\nwant %q", held, rows(out), want)
		}
	}

	// What a sensor reads was set by nobody: it is printed to the tenth, as
	// it is.
	fake.Sensors["tact"] = "2942"

	if out, _, _ = run(t, fake, warm(), "status"); !strings.Contains(rows(out), "temperature: 21.1C, 69.9F\n") {
		t.Errorf("a room at 2942 tenths of a kelvin is printed in\n%s", rows(out))
	}
}

// TestHotCoolVariant: a variant of the type 527 is a Hot+Cool all the same:
// its status is printed as one, and so is what it says under watch.
func TestHotCoolVariant(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCoolVariant("K")
	t.Cleanup(fake.Close)

	env := map[string]string{
		cli.EnvSSID: dysontest.HotCoolSSID + "K", cli.EnvPassword: dysontest.HotCoolWiFiPassword,
	}

	printed, errs, status := run(t, fake, env, "status")
	if status != cli.ExitOK || errs != "" ||
		!strings.Contains(rows(printed), "filters: HEPA 95% left, carbon 80% left\nheat: off, to 20.9C, 69.5F\n") ||
		!strings.Contains(rows(printed), "PM2.5: 7 ug/m3") || strings.Contains(printed, "hours left") {
		t.Errorf("status of a 527K exits %d, saying %q and\n%s", status, errs, rows(printed))
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	var (
		stdout out
		stderr bytes.Buffer
		ended  = make(chan int, 1)
	)

	go func() {
		ended <- cli.Run(ctx, cli.Environment{
			Args: []string{"--host", fake.Addr(), "watch"}, Out: &stdout, Err: &stderr,
			Getenv: func(name string) string { return env[name] },
		})
	}()

	// The state at once, then what the sensors say when they speak.
	deadline := time.Now().Add(5 * time.Second)
	for len(stdout.lines()) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	fake.Say("ENVIRONMENTAL-CURRENT-SENSOR-DATA", "data", map[string]string{"p25r": "0012", "va10": "0031"})

	for len(stdout.lines()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	stop()

	if exit := <-ended; exit != cli.ExitOK {
		t.Errorf("watch, told to stop, exits %d saying %q", exit, stderr.String())
	}

	if lines := stdout.lines(); len(lines) != 2 ||
		!strings.Contains(lines[0], "filters HEPA 95% left, carbon 80% left; heat off, to 20.9C, 69.5F") ||
		!strings.Contains(lines[1], "PM2.5 12 ug/m3") || !strings.Contains(lines[1], "volatile compounds 3.1") {
		t.Errorf("watch of a 527K printed %q", lines)
	}
}

// TestHeat: heat sends its one change to a Hot+Cool: to heat, to a
// temperature, to heat no more. What is no temperature is refused, and so
// is one out of range, with nothing sent; a Link is not told to heat.
func TestHeat(t *testing.T) {
	t.Parallel()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	for _, step := range []struct {
		args []string
		want map[string]string
	}{
		{[]string{"heat", "on"}, map[string]string{"hmod": "HEAT"}},
		{[]string{"heat", "21.5"}, map[string]string{"hmod": "HEAT", "hmax": "2947"}},
		{[]string{"heat", "off"}, map[string]string{"hmod": "OFF"}},
	} {
		before := len(fake.Received())

		out, errs, status := run(t, fake, warm(), step.args...)
		if status != cli.ExitOK || out != "" || errs != "" {
			t.Fatalf("%v exits %d, saying %q and %q", step.args, status, out, errs)
		}

		sent := fake.Received()[before:]
		if len(sent) != 1 || sent[0].Message != "STATE-SET" || !reflect.DeepEqual(sent[0].Data, step.want) {
			t.Errorf("%v sent %+v, want a STATE-SET of %v", step.args, sent, step.want)
		}
	}

	before := len(fake.Received())

	for _, row := range []struct {
		args []string
		want string
	}{
		{[]string{"heat", "warm"}, `"warm" is neither a temperature in Celsius, nor on, nor off`},
		{[]string{"heat", "40"}, "a heat target of 40, outside 0.85 to 36.85 Celsius"},
	} {
		out, errs, status := run(t, fake, warm(), row.args...)
		if status != cli.ExitError || out != "" || !strings.Contains(errs, row.want) {
			t.Errorf("%v exits %d, saying %q and %q, want an error with %q", row.args, status, out, errs, row.want)
		}
	}

	if sent := fake.Received()[before:]; len(sent) != 0 {
		t.Errorf("the refused heats sent %+v", sent)
	}

	link := started(t)

	out, errs, status := run(t, link, sticker(), "heat", "on")
	if status != cli.ExitError || out != "" || !strings.Contains(errs, "it has no heater") {
		t.Errorf("heat on a Link exits %d, saying %q and %q", status, out, errs)
	}

	if sent := link.Received(); len(sent) != 0 {
		t.Errorf("a Link told to heat was sent %+v", sent)
	}
}

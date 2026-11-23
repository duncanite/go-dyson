package dyson_test

// The generation after the Link's, the type 527, from libdyson-neon, each
// rule under the file it is in.
//
// dyson_pure_cool.py: "fpwr" is the fan's power and "auto" its auto mode,
// each ON or OFF; the speed is set with "fpwr" ON. "oson" is the
// oscillation, written ON and OFF or OION and OIOF by the firmware, and
// set in the words the purifier uses, with "fpwr" ON, "ancp" CUST and
// the angles "osal" and "osau", four digits each; stopped with "oson"
// alone. This package says "ancp" back as the purifier last said it, CUST
// or a sweep of its own, where the reference always says CUST. "hflr" and "cflr" are the filters' lives in percent,
// "cflr" INV
// when there is no carbon filter. "p25r" or "pm25", and "p10r" or
// "pm10", are the particulates; "va10" and "noxl" the indices of volatile
// compounds and of nitrogen dioxide, in tenths.
//
// dyson_device.py, DysonHeatingDevice: "hmod" is HEAT or OFF, "hsta" HEAT
// while it heats, "hmax" the target in tenths of a kelvin; the target is
// set with "hmod" HEAT and "hmax" as four digits, 274 to 310 kelvin.

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-dyson/mqtt"
)

func hotCool(t *testing.T) (*dysontest.Purifier, *dyson.Purifier) {
	t.Helper()

	fake := dysontest.NewHotCool()
	t.Cleanup(fake.Close)

	identity, err := dyson.FromSticker(dysontest.HotCoolSSID, dysontest.HotCoolWiFiPassword)
	if err != nil {
		t.Fatalf("FromSticker: %v", err)
	}

	purifier, err := dyson.Dial(soon(t), fake.Addr(), identity)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	t.Cleanup(func() { _ = purifier.Close() })

	return fake, purifier
}

// near is whether a reading is there and is what is wanted.
func near(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 1e-9
}

// TestHotCoolDial: a Hot+Cool is spoken to on its own type's topics, and
// so are its variants; a type of another generation is still refused.
func TestHotCoolDial(t *testing.T) {
	t.Parallel()

	fake, _ := hotCool(t)

	if filters := fake.Filters(); len(filters) != 1 || filters[0] != "527/"+dysontest.HotCoolSerial+"/status/current" {
		t.Errorf("subscribed to %v, want the type 527's status", filters)
	}

	identity, err := dyson.FromSticker(dysontest.HotCoolSSID, dysontest.HotCoolWiFiPassword)
	if err != nil {
		t.Fatalf("FromSticker: %v", err)
	}

	for _, variant := range []string{"527E", "527K", "527M"} {
		identity.Type = variant

		purifier, failed := dyson.Dial(soon(t), fake.Addr(), identity)
		if failed != nil {
			t.Errorf("Dial of the type %s: %v", variant, failed)

			continue
		}

		_ = purifier.Close()
	}

	for _, other := range []string{"527X", "438", "358", "52"} {
		identity.Type = other

		if _, err = dyson.Dial(soon(t), fake.Addr(), identity); !errors.Is(err, dyson.ErrUnsupported) {
			t.Errorf("Dial of the type %s = %v, want ErrUnsupported", other, err)
		}
	}
}

// TestHotCoolReading: the state and the sensors of a Hot+Cool, read from
// its own fields: the power and the auto mode as one mode, the two
// filters, the heater and its target, the particulates under either name,
// the indices in tenths; what is off, starting, failed or not there reads
// as nothing, and a Link's fields as nothing.
func TestHotCoolReading(t *testing.T) {
	t.Parallel()

	fake, purifier := hotCool(t)
	ctx := soon(t)

	state, err := purifier.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	heat := state.Heat
	state.Heat = nil

	if want := (dyson.State{
		Mode: dyson.Fan, Running: true, Speed: 5, Oscillating: true, Monitoring: true, HEPA: new(95), Carbon: new(80),
	}); !reflect.DeepEqual(state, want) || heat == nil || heat.On || heat.Heating || !near(heat.Target, 20.8) {
		t.Errorf("State = %+v, heat %+v\nwant %+v, the heater off with a target of 20.8", state, heat, want)
	}

	environment, err := purifier.Environment(ctx)
	if err != nil || !near(environment.Temperature, 21.85) || !reflect.DeepEqual(environment.Humidity, new(45)) ||
		!reflect.DeepEqual(environment.PM25, new(7)) || !reflect.DeepEqual(environment.PM10, new(10)) ||
		!near(environment.VOCIndex, 0.4) || !near(environment.NO2Index, 1.2) ||
		environment.Particulates != nil || environment.VOC != nil || environment.SleepTimer != 0 {
		t.Errorf("Environment = %+v, %v", environment, err)
	}

	// Off; then in auto, heating, with no carbon filter, the oscillation
	// written the older way, and the particulates under their other name.
	fake.Product["fpwr"], fake.Product["fnst"] = "OFF", "OFF"

	if state, err = purifier.State(ctx); err != nil || state.Mode != dyson.Off || state.Running {
		t.Errorf("State of a Hot+Cool that is off = %+v, %v", state, err)
	}

	fake.Product["fpwr"], fake.Product["auto"], fake.Product["fnsp"] = "ON", "ON", "AUTO"
	fake.Product["oson"], fake.Product["cflr"], fake.Product["hflr"] = "ON", "INV", "0003"
	fake.Product["hmod"], fake.Product["hsta"], fake.Product["hmax"] = "HEAT", "HEAT", "2980"
	fake.Product["ercd"], fake.Product["wacd"] = "11E1", "FLTR"
	fake.Sensors["va10"], fake.Sensors["noxl"], fake.Sensors["tact"] = "INIT", "FAIL", "OFF"
	fake.Sensors["sltm"] = "0015"
	delete(fake.Sensors, "p25r")
	delete(fake.Sensors, "p10r")

	if state, err = purifier.State(ctx); err != nil {
		t.Fatalf("State: %v", err)
	}

	heat, state.Heat = state.Heat, nil

	if want := (dyson.State{
		Mode: dyson.Auto, Oscillating: true, Monitoring: true, HEPA: new(3), Error: "11E1", Warning: "FLTR",
	}); !reflect.DeepEqual(state, want) || heat == nil || !heat.On || !heat.Heating || !near(heat.Target, 24.8) {
		t.Errorf("State in auto, heating = %+v, heat %+v\nwant %+v, heating to 24.8", state, heat, want)
	}

	environment, err = purifier.Environment(ctx)
	if want := (dyson.Environment{
		Humidity: new(45), PM25: new(6), PM10: new(9), SleepTimer: 15 * time.Minute,
	}); err != nil || !reflect.DeepEqual(environment, want) {
		t.Errorf("Environment with sensors off = %+v, %v\nwant %+v", environment, err, want)
	}

	// A heater that says no target, and a state that says nothing of one.
	fake.Product["hmax"] = "OFF"

	if state, err = purifier.State(ctx); err != nil || state.Heat == nil || state.Heat.Target != nil {
		t.Errorf("State with no heat target = %+v, %v", state.Heat, err)
	}

	delete(fake.Product, "hmod")

	if state, err = purifier.State(ctx); err != nil || state.Heat != nil {
		t.Errorf("State that says nothing of a heater = %+v, %v", state.Heat, err)
	}

	fake.Product["fpwr"] = "MAYBE"

	if _, err = purifier.State(ctx); !errors.Is(err, dyson.ErrAnswer) {
		t.Errorf("State whose power is no word = %v, want ErrAnswer", err)
	}
}

// TestHotCoolSetting: each change to a Hot+Cool is one STATE-SET with the
// fields its generation reads. The oscillation is asked for in the words
// the purifier last used, with its angles and the fan on, and a purifier
// that has said no state yet is asked for one first.
func TestHotCoolSetting(t *testing.T) {
	t.Parallel()

	fake, purifier := hotCool(t)
	ctx := soon(t)

	sets := func() []dysontest.Received {
		var sent []dysontest.Received

		for _, one := range fake.Received() {
			if one.Message == "STATE-SET" {
				sent = append(sent, one)
			}
		}

		return sent
	}

	// Nothing was read yet: the purifier is asked how it stands, then told.
	if err := purifier.SetOscillation(ctx, true); err != nil {
		t.Fatalf("SetOscillation: %v", err)
	}

	if asked := fake.Received(); len(asked) != 2 || asked[0].Message != "REQUEST-CURRENT-STATE" ||
		!reflect.DeepEqual(asked[1].Data, map[string]string{
			"oson": "OION", "fpwr": "ON", "ancp": "CUST", "osal": "0063", "osau": "0243",
		}) {
		t.Fatalf("turning a purifier that had said nothing sent %+v", asked)
	}

	// The purifier says each change, which is read before the next.
	said := func(after string) {
		t.Helper()

		if update, err := purifier.Next(ctx); err != nil || update.State == nil {
			t.Fatalf("Next after %s = %+v, %v", after, update, err)
		}
	}

	// What SetOscillation read to learn how the purifier stands is Next's
	// to give all the same, before the change that followed.
	said("it was asked how it stands")
	said("it was told to turn")

	for _, step := range []struct {
		name string
		do   func() error
		want map[string]string
	}{
		{"still", func() error { return purifier.SetOscillation(ctx, false) }, map[string]string{"oson": "OIOF"}},
		{"off", func() error { return purifier.SetMode(ctx, dyson.Off) }, map[string]string{"fpwr": "OFF"}},
		{"on", func() error { return purifier.SetMode(ctx, dyson.Fan) }, map[string]string{"fpwr": "ON", "auto": "OFF"}},
		{"auto", func() error { return purifier.SetMode(ctx, dyson.Auto) }, map[string]string{"fpwr": "ON", "auto": "ON"}},
		{"speed", func() error { return purifier.SetSpeed(ctx, 7) }, map[string]string{"fpwr": "ON", "fnsp": "0007"}},
		{"night", func() error { return purifier.SetNightMode(ctx, true) }, map[string]string{"nmod": "ON"}},
		{"timer", func() error { return purifier.SetSleepTimer(ctx, time.Hour) }, map[string]string{"sltm": "0060"}},
		{"heat", func() error { return purifier.SetHeat(ctx, true) }, map[string]string{"hmod": "HEAT"}},
		{
			"heat to the lowest", func() error { return purifier.SetHeatTarget(ctx, dyson.MinHeatTarget) },
			map[string]string{"hmod": "HEAT", "hmax": "2740"},
		},
		{
			"heat to the highest", func() error { return purifier.SetHeatTarget(ctx, dyson.MaxHeatTarget) },
			map[string]string{"hmod": "HEAT", "hmax": "3100"},
		},
		{
			"heat to a tenth", func() error { return purifier.SetHeatTarget(ctx, 21.5) },
			map[string]string{"hmod": "HEAT", "hmax": "2947"},
		},
		{
			// 55 and 98 degrees Fahrenheit, as a purifier set in that scale is.
			"heat to 55F", func() error { return purifier.SetHeatTarget(ctx, (55.0-32)*5/9) },
			map[string]string{"hmod": "HEAT", "hmax": "2859"},
		},
		{
			"heat to 98F", func() error { return purifier.SetHeatTarget(ctx, (98.0-32)*5/9) },
			map[string]string{"hmod": "HEAT", "hmax": "3098"},
		},
		{"heat no more", func() error { return purifier.SetHeat(ctx, false) }, map[string]string{"hmod": "OFF"}},
	} {
		before := len(sets())

		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}

		sent := sets()[before:]
		if len(sent) != 1 || sent[0].Reason != "LAPP" || sent[0].QoS != mqtt.AtLeastOnce ||
			!reflect.DeepEqual(sent[0].Data, step.want) {
			t.Errorf("%s sent %+v, want a STATE-SET of %v", step.name, sent, step.want)
		}

		said(step.name)
	}

	// A purifier that writes its oscillation the older way is told in it,
	// and one set to a sweep of its own is told that sweep back.
	fake.Product["oson"], fake.Product["ancp"] = "OFF", "0180"
	delete(fake.Product, "osal")

	if _, err := purifier.State(ctx); err != nil {
		t.Fatalf("State: %v", err)
	}

	before := len(sets())

	if err := purifier.SetOscillation(ctx, true); err != nil {
		t.Fatalf("SetOscillation: %v", err)
	}

	if err := purifier.SetOscillation(ctx, false); err != nil {
		t.Fatalf("SetOscillation: %v", err)
	}

	if sent := sets()[before:]; len(sent) != 2 ||
		!reflect.DeepEqual(sent[0].Data, map[string]string{
			"oson": "ON", "fpwr": "ON", "ancp": "0180", "osal": "0063", "osau": "0243",
		}) || !reflect.DeepEqual(sent[1].Data, map[string]string{"oson": "OFF"}) {
		t.Errorf("turning and stopping a purifier that writes ON and OFF sent %+v", sent)
	}

	// A purifier whose state writes no oscillation at all is told in the
	// usual words, once it has said that state: the setting does not wait
	// for a word that will not come.
	wordless, quiet := hotCool(t)
	delete(wordless.Product, "oson")

	if err := quiet.SetOscillation(ctx, true); err != nil {
		t.Fatalf("SetOscillation of a purifier that writes no oscillation: %v", err)
	}

	if asked := wordless.Received(); len(asked) != 2 || asked[0].Message != "REQUEST-CURRENT-STATE" ||
		asked[1].Data["oson"] != "ON" {
		t.Errorf("turning a purifier that writes no oscillation sent %+v", asked)
	}

	sent := len(fake.Received())

	for name, do := range map[string]func() error{
		"a heat target under the lowest":  func() error { return purifier.SetHeatTarget(ctx, 0.7) },
		"a heat target over the highest":  func() error { return purifier.SetHeatTarget(ctx, 37) },
		"a heat target with no end":       func() error { return purifier.SetHeatTarget(ctx, math.Inf(1)) },
		"a heat target that is no number": func() error { return purifier.SetHeatTarget(ctx, math.NaN()) },
		"a speed of eleven":               func() error { return purifier.SetSpeed(ctx, 11) },
	} {
		if err := do(); !errors.Is(err, dyson.ErrArgument) {
			t.Errorf("%s = %v, want ErrArgument", name, err)
		}
	}

	if len(fake.Received()) != sent {
		t.Errorf("the refused changes sent %+v", fake.Received()[sent:])
	}
}

// TestNoHeater: a Link has no heater: it is not told to heat, and its
// state says nothing of one, nor of the filters of another generation.
func TestNoHeater(t *testing.T) {
	t.Parallel()

	fake, purifier := connected(t)
	ctx := soon(t)

	if err := purifier.SetHeat(ctx, true); !errors.Is(err, dyson.ErrUnsupported) {
		t.Errorf("SetHeat on a Link = %v, want ErrUnsupported", err)
	}

	if err := purifier.SetHeatTarget(ctx, 21); !errors.Is(err, dyson.ErrUnsupported) {
		t.Errorf("SetHeatTarget on a Link = %v, want ErrUnsupported", err)
	}

	if sent := fake.Received(); len(sent) != 0 {
		t.Errorf("a Link told to heat was sent %+v", sent)
	}

	state, err := purifier.State(ctx)
	if err != nil || state.Heat != nil || state.HEPA != nil || state.Carbon != nil {
		t.Errorf("State of a Link = %+v, %v", state, err)
	}
}

// TestAHeatTargetReadsBackAsSet: a target set to a tenth of a degree is
// the target the purifier then says, the very number: at both ends of
// the range, at whole degrees, and at tenths a float writes a hair
// under. What the purifier holds is TargetHeld above it.
func TestAHeatTargetReadsBackAsSet(t *testing.T) {
	t.Parallel()

	fake, purifier := hotCool(t)
	ctx := soon(t)

	for _, tenths := range []int{8, 9, 100, 199, 210, 250, 253, 287, 299, 301, 367, 368} {
		set := float64(tenths) / 10

		if err := purifier.SetHeatTarget(ctx, set); err != nil {
			t.Fatalf("SetHeatTarget(%v): %v", set, err)
		}

		var read *float64

		for read == nil {
			update, err := purifier.Next(ctx)
			if err != nil {
				t.Fatalf("Next after SetHeatTarget(%v): %v", set, err)
			}

			if update.State != nil && update.State.Heat != nil {
				read = update.State.Heat.Target
			}
		}

		// The same number, not one near it.
		if *read != set {
			t.Errorf("a target set to %v reads back as %v", set, *read)
		}

		// A target is held in tenths of a kelvin: 273.15 above, and
		// TargetHeld above that.
		sent := fake.Received()
		if held := sent[len(sent)-1].Data["hmax"]; held != fmt.Sprintf("%04d", tenths+2732) {
			t.Errorf("a target of %v was sent as %s", set, held)
		}
	}

	// What arithmetic left a billionth under 25.3 is 25.3 to whoever made
	// it, and is sent as that tenth, not as the one below.
	under := 25.3 - 1e-9

	if err := purifier.SetHeatTarget(ctx, under); err != nil {
		t.Fatal(err)
	}

	if sent := fake.Received(); sent[len(sent)-1].Data["hmax"] != "2985" {
		t.Errorf("a target a hair under 25.3 was sent as %s", sent[len(sent)-1].Data["hmax"])
	}

	if dyson.TargetHeld != 0.05 || dyson.MinHeatTarget != 0.8 || dyson.MaxHeatTarget != 36.8 {
		t.Errorf("the target's bounds and what is held above it: %v, %v, %v",
			dyson.MinHeatTarget, dyson.MaxHeatTarget, dyson.TargetHeld)
	}
}

package dyson_test

// The contract, from libdyson-neon
// (https://github.com/libdyson-wg/libdyson-neon), the reference
// implementation, each rule under the file it is in.
//
// utils.py: a purifier's own network name is "DYSON-<serial>-<type>"; its
// credential is the base64 of the SHA-512 of the Wi-Fi password on its
// sticker; the type 455A is 455 on its topics.
//
// dyson_device.py: the purifier's broker takes MQTT 3.1, the serial as
// user name and the credential as password. It says its state on
// "<type>/<serial>/status/current" and is spoken to on
// "<type>/<serial>/command", in JSON with "msg" and "time".
// REQUEST-CURRENT-STATE is answered by CURRENT-STATE, its fields under
// "product-state"; REQUEST-PRODUCT-ENVIRONMENT-CURRENT-SENSOR-DATA by
// ENVIRONMENTAL-CURRENT-SENSOR-DATA, its fields under "data". A change is
// asked with STATE-SET, "mode-reason" LAPP and the fields under "data",
// published at quality of service 1, and said with STATE-CHANGE, each
// field then a pair, the value before and the value now. A sensor's field
// is a number, or OFF, INIT or FAIL. "tact" is the temperature in tenths
// of a kelvin, "hact" the humidity in percent, "sltm" the sleep timer in
// minutes, set as four digits, 1 to 540, or OFF.
//
// dyson_pure_cool_link.py, the type 475: "fmod" is FAN, AUTO or OFF;
// "fnst" is FAN when the fan turns; "fnsp" the speed, four digits, 1 to
// 10, set with "fmod" FAN; "oson", "nmod" and "rhtm" are ON or OFF;
// "filf" the filter's hours left; "pact" and "vact" the particulates and
// the volatile compounds.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/dysontest"
	"github.com/duncanite/go-dyson/mqtt"
)

// credential is the base64 of the SHA-512 of dysontest.WiFiPassword, as
// openssl computes it.
const credential = "YaPzmo9gsunumT9kc/lq9LNbgznmt5XMnn2thESzz0+1lFH8Xv1iZ5QQ0STXG0At0NEwwinh4dnp5dlkz8t7Aw=="

func soon(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func connected(t *testing.T) (*dysontest.Purifier, *dyson.Purifier) {
	t.Helper()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	identity, err := dyson.FromSticker(dysontest.SSID, dysontest.WiFiPassword)
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

// TestEnded: a Purifier says when its connection is over, and why, with
// nothing read from it: nothing while it lives; mqtt.ErrLost when the
// purifier went; mqtt.ErrClosed when it was closed.
func TestEnded(t *testing.T) {
	t.Parallel()

	fake, purifier := connected(t)

	select {
	case <-purifier.Done():
		t.Fatalf("a Purifier that lives is done: %v", purifier.Err())
	default:
	}

	if err := purifier.Err(); err != nil {
		t.Errorf("Err of a Purifier that lives = %v", err)
	}

	fake.Drop()

	select {
	case <-purifier.Done():
	case <-soon(t).Done():
		t.Fatal("a Purifier whose purifier went is not done")
	}

	if err := purifier.Err(); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("Err after the purifier went = %v, want mqtt.ErrLost", err)
	}

	_, closed := connected(t)

	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-closed.Done():
	case <-soon(t).Done():
		t.Fatal("a Purifier that was closed is not done")
	}

	if err := closed.Err(); !errors.Is(err, mqtt.ErrClosed) {
		t.Errorf("Err after Close = %v, want mqtt.ErrClosed", err)
	}
}

// TestSticker: a sticker's network name and password give the serial, the
// type and the credential; a name that is no Dyson sticker's is
// ErrSticker; an Identity shows nothing of its credential.
func TestSticker(t *testing.T) {
	t.Parallel()

	identity, err := dyson.FromSticker(dysontest.SSID, dysontest.WiFiPassword)
	if want := (dyson.Identity{Serial: dysontest.Serial, Type: "475", Credential: credential}); err != nil ||
		identity != want {
		t.Fatalf("FromSticker = %#v, %v", identity.Serial+" "+identity.Type+" "+identity.Credential, err)
	}

	other, err := dyson.FromSticker("DYSON-AB1-EU-ABC1234Z-455A", "x")
	if err != nil || other.Type != "455" {
		t.Errorf("FromSticker of a 455A = %q, %v; want the type 455", other.Type, err)
	}

	for _, ssid := range []string{
		"", "dyson-NN2-US-HKA0000A-475", "DYSON-NN2-US-HKA0000A", "DYSON-NN2-US-HKA0000A-47", "MyNetwork",
		" DYSON-NN2-US-HKA0000A-475", "DYSON-NN2-US-HKA0000A-475\n", "360EYE-NN2-US-HKA0000A",
	} {
		if _, err = dyson.FromSticker(ssid, "x"); !errors.Is(err, dyson.ErrSticker) {
			t.Errorf("FromSticker(%q) = %v, want ErrSticker", ssid, err)
		}
	}

	// The two values the wrong way round: the password is not shown.
	if _, err = dyson.FromSticker(dysontest.WiFiPassword, dysontest.SSID); !errors.Is(err, dyson.ErrSticker) ||
		strings.Contains(err.Error(), dysontest.WiFiPassword) {
		t.Errorf("FromSticker with the password where the name goes = %v", err)
	}

	type holder struct {
		Identity dyson.Identity `json:"identity"`
	}

	marshalled, err := json.Marshal(holder{identity})
	if err != nil || strings.Contains(string(marshalled), credential) ||
		!strings.Contains(string(marshalled), dysontest.Serial) {
		t.Errorf("a struct that holds an Identity, as JSON = %s, %v", marshalled, err)
	}

	if _, err = dyson.FromSticker(dysontest.SSID, ""); !errors.Is(err, dyson.ErrArgument) {
		t.Errorf("FromSticker with no password = %v, want ErrArgument", err)
	}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		printed := fmt.Sprintf(verb, identity) + fmt.Sprintf(verb, &identity) + identity.LogValue().String()
		if strings.Contains(printed, credential) || strings.Contains(printed, fmt.Sprintf("%x", credential)) ||
			!strings.Contains(printed, dysontest.Serial) {
			t.Errorf("an Identity under %s prints %s", verb, printed)
		}
	}
}

// TestDial: the session a purifier's broker takes is opened, and its
// status subscribed to; a wrong credential is the broker's refusal; a
// type this package does not speak, and an identity that is not whole,
// send nothing.
func TestDial(t *testing.T) {
	t.Parallel()

	fake, _ := connected(t)

	hellos := fake.Hellos()
	if len(hellos) != 1 || hellos[0].Protocol != "MQIsdp" || hellos[0].Version != 3 ||
		hellos[0].Username != dysontest.Serial || hellos[0].Password != credential {
		t.Errorf("the session opened as %+v", hellos)
	}

	if filters := fake.Filters(); len(filters) != 1 || filters[0] != dysontest.Status {
		t.Errorf("subscribed to %v, want %s", filters, dysontest.Status)
	}

	wrong, err := dyson.FromSticker(dysontest.SSID, "another password")
	if err != nil {
		t.Fatalf("FromSticker: %v", err)
	}

	if _, err = dyson.Dial(soon(t), fake.Addr(), wrong); !errors.Is(err, mqtt.ErrCredentials) ||
		strings.Contains(err.Error(), wrong.Credential) {
		t.Errorf("Dial with another sticker's password = %v, want mqtt.ErrCredentials", err)
	}

	sessions := len(fake.Hellos())
	whole := dyson.Identity{Serial: dysontest.Serial, Type: dyson.PureCoolLink, Credential: credential}

	later := whole
	later.Type = "438"

	if _, err = dyson.Dial(soon(t), fake.Addr(), later); !errors.Is(err, dyson.ErrUnsupported) {
		t.Errorf("Dial of a later generation = %v, want ErrUnsupported", err)
	}

	for name, each := range map[string]struct {
		host     string
		identity dyson.Identity
	}{
		"no host":       {"", whole},
		"no serial":     {fake.Addr(), dyson.Identity{Type: dyson.PureCoolLink, Credential: credential}},
		"no credential": {fake.Addr(), dyson.Identity{Serial: dysontest.Serial, Type: dyson.PureCoolLink}},
	} {
		if _, err = dyson.Dial(soon(t), each.host, each.identity); !errors.Is(err, dyson.ErrArgument) {
			t.Errorf("Dial with %s = %v, want ErrArgument", name, err)
		}
	}

	if len(fake.Hellos()) != sessions {
		t.Errorf("the refused identities opened sessions: %+v", fake.Hellos()[sessions:])
	}
}

// TestReading: the state and the sensors, each asked for by its message,
// with the time, and read from the answer; a sensor that is off, starting
// or failed reads as nothing.
func TestReading(t *testing.T) {
	t.Parallel()

	fake, purifier := connected(t)
	ctx := soon(t)

	state, err := purifier.State(ctx)
	if want := (dyson.State{
		Mode: dyson.Fan, Running: true, Speed: 4, Oscillating: true, Monitoring: true,
		FilterLife: 2159 * time.Hour,
	}); err != nil || state != want {
		t.Errorf("State = %+v, %v\nwant %+v", state, err, want)
	}

	environment, err := purifier.Environment(ctx)
	if err != nil || environment.Temperature == nil || math.Abs(*environment.Temperature-24.55) > 1e-9 ||
		!reflect.DeepEqual(environment.Humidity, new(58)) || !reflect.DeepEqual(environment.Particulates, new(0)) ||
		!reflect.DeepEqual(environment.VOC, new(3)) || environment.SleepTimer != 0 {
		t.Errorf("Environment = %+v, %v", environment, err)
	}

	asked := fake.Received()
	if len(asked) != 2 || asked[0].Message != "REQUEST-CURRENT-STATE" || asked[0].QoS != mqtt.AtMostOnce ||
		asked[1].Message != "REQUEST-PRODUCT-ENVIRONMENT-CURRENT-SENSOR-DATA" || asked[0].Data != nil {
		t.Errorf("the purifier was asked %+v", asked)
	}

	at, err := time.Parse("2006-01-02T15:04:05Z", asked[0].Time)
	if err != nil || time.Since(at) > time.Minute {
		t.Errorf("the request's time is %q", asked[0].Time)
	}

	fake.Product["fmod"], fake.Product["fnst"], fake.Product["fnsp"] = "AUTO", "OFF", "AUTO"
	fake.Product["ercd"], fake.Product["wacd"], fake.Product["nmod"] = "02C0", "FLTR", "ON"
	fake.Sensors["tact"], fake.Sensors["hact"], fake.Sensors["pact"] = "OFF", "INIT", "FAIL"
	fake.Sensors["sltm"] = "0090"
	delete(fake.Sensors, "vact")

	state, err = purifier.State(ctx)
	if want := (dyson.State{
		Mode: dyson.Auto, Oscillating: true, NightMode: true, Monitoring: true, FilterLife: 2159 * time.Hour,
		Error: "02C0", Warning: "FLTR",
	}); err != nil || state != want {
		t.Errorf("State in auto, with codes = %+v, %v\nwant %+v", state, err, want)
	}

	environment, err = purifier.Environment(ctx)
	if want := (dyson.Environment{SleepTimer: 90 * time.Minute}); err != nil || environment != want {
		t.Errorf("Environment with no sensor reading = %+v, %v", environment, err)
	}
}

// TestSetting: each change is one STATE-SET, as the app's, acknowledged,
// with the fields the purifier reads; the purifier takes it, and says so.
func TestSetting(t *testing.T) {
	t.Parallel()

	fake, purifier := connected(t)
	ctx := soon(t)

	for _, step := range []struct {
		do   func() error
		want map[string]string
	}{
		{func() error { return purifier.SetMode(ctx, dyson.Off) }, map[string]string{"fmod": "OFF"}},
		{func() error { return purifier.SetMode(ctx, dyson.Auto) }, map[string]string{"fmod": "AUTO"}},
		{func() error { return purifier.SetSpeed(ctx, 1) }, map[string]string{"fmod": "FAN", "fnsp": "0001"}},
		{func() error { return purifier.SetSpeed(ctx, 10) }, map[string]string{"fmod": "FAN", "fnsp": "0010"}},
		{func() error { return purifier.SetOscillation(ctx, false) }, map[string]string{"oson": "OFF"}},
		{func() error { return purifier.SetNightMode(ctx, true) }, map[string]string{"nmod": "ON"}},
		{func() error { return purifier.SetSleepTimer(ctx, 90*time.Minute) }, map[string]string{"sltm": "0090"}},
		{func() error { return purifier.SetSleepTimer(ctx, dyson.MaxSleepTimer) }, map[string]string{"sltm": "0540"}},
		{func() error { return purifier.SetSleepTimer(ctx, 0) }, map[string]string{"sltm": "OFF"}},
	} {
		before := len(fake.Received())

		if err := step.do(); err != nil {
			t.Fatalf("the change to %v: %v", step.want, err)
		}

		sent := fake.Received()[before:]
		if len(sent) != 1 || sent[0].Message != "STATE-SET" || sent[0].Reason != "LAPP" ||
			sent[0].QoS != mqtt.AtLeastOnce || !reflect.DeepEqual(sent[0].Data, step.want) {
			t.Errorf("sent %+v, want a STATE-SET of %v", sent, step.want)
		}

		// The purifier says each change: the value before and the value now.
		update, err := purifier.Next(ctx)
		if err != nil || update.State == nil || update.Environment != nil {
			t.Fatalf("Next after the change to %v = %+v, %v", step.want, update, err)
		}
	}

	state, err := purifier.State(ctx)
	if want := (dyson.State{
		Mode: dyson.Fan, Running: true, Speed: 10, NightMode: true, Monitoring: true, FilterLife: 2159 * time.Hour,
	}); err != nil || state != want {
		t.Errorf("State after the changes = %+v, %v\nwant %+v", state, err, want)
	}

	sent := len(fake.Received())

	for name, do := range map[string]func() error{
		"a mode that is none":    func() error { return purifier.SetMode(ctx, "TURBO") },
		"a speed of zero":        func() error { return purifier.SetSpeed(ctx, 0) },
		"a speed of eleven":      func() error { return purifier.SetSpeed(ctx, 11) },
		"a timer under a minute": func() error { return purifier.SetSleepTimer(ctx, time.Second) },
		"a timer over nine hours": func() error {
			return purifier.SetSleepTimer(ctx, dyson.MaxSleepTimer+time.Minute)
		},
		"a timer below zero": func() error { return purifier.SetSleepTimer(ctx, -time.Minute) },
	} {
		if err := do(); !errors.Is(err, dyson.ErrArgument) {
			t.Errorf("%s = %v, want ErrArgument", name, err)
		}
	}

	if len(fake.Received()) != sent {
		t.Errorf("the refused changes sent %+v", fake.Received()[sent:])
	}
}

// TestListening: what the purifier says by itself is read as it comes: a
// change made at the purifier, its fields pairs; the sensors; a message
// of another kind passed over. What is not the documented JSON is
// ErrAnswer. A wait ends with its context, and with the connection.
func TestListening(t *testing.T) {
	t.Parallel()

	fake, purifier := connected(t)
	ctx := soon(t)

	fake.Say("LOCATION", "data", map[string]string{"lat": "0"})
	fake.Set("oson", "OFF")

	update, err := purifier.Next(ctx)
	if err != nil || update.State == nil || update.State.Oscillating || update.State.Speed != 4 ||
		update.State.Mode != dyson.Fan {
		t.Fatalf("Next after a change at the purifier = %+v, %v", update.State, err)
	}

	fake.Say("ENVIRONMENTAL-CURRENT-SENSOR-DATA", "data", map[string]string{"hact": "0041"})

	update, err = purifier.Next(ctx)
	if err != nil || update.Environment == nil || !reflect.DeepEqual(update.Environment.Humidity, new(41)) ||
		update.Environment.Temperature != nil {
		t.Fatalf("Next after the sensors spoke = %+v, %v", update.Environment, err)
	}

	fake.Say("CURRENT-STATE", "product-state", map[string]string{"fnsp": "0004"})

	if _, err = purifier.Next(ctx); !errors.Is(err, dyson.ErrAnswer) {
		t.Errorf("Next of a state with no mode = %v, want ErrAnswer", err)
	}

	fake.Publish(mqtt.Message{Topic: dysontest.Status, Payload: []byte(`not JSON`)})

	if _, err = purifier.Next(ctx); !errors.Is(err, dyson.ErrAnswer) {
		t.Errorf("Next of what is not JSON = %v, want ErrAnswer", err)
	}

	brief, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	if _, err = purifier.Next(brief); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Next with nothing said = %v, want the deadline", err)
	}

	fake.Drop()

	if _, err = purifier.Next(ctx); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("Next after the purifier went = %v, want mqtt.ErrLost", err)
	}

	if _, err = purifier.State(ctx); !errors.Is(err, mqtt.ErrLost) {
		t.Errorf("State after the purifier went = %v, want mqtt.ErrLost", err)
	}

	if err = purifier.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

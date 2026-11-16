package dyson

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Mode is what the fan is set to do.
type Mode string

// The modes of a Link purifier.
const (
	// Off is the fan off; the sensors may go on reading.
	Off Mode = "OFF"
	// Fan is the fan on, at the speed set.
	Fan Mode = "FAN"
	// Auto is the fan run by the purifier, on when the air needs it.
	Auto Mode = "AUTO"
)

// The fan's speeds, and the longest sleep timer.
const (
	MinSpeed      = 1
	MaxSpeed      = 10
	MaxSleepTimer = 540 * time.Minute
)

// State is how a purifier is set and what it is doing.
type State struct {
	// Mode is what the fan is set to do.
	Mode Mode
	// Running is whether the fan turns: in Auto it may not.
	Running bool
	// Speed is the speed set, MinSpeed to MaxSpeed; zero when the purifier
	// chooses it.
	Speed int
	// Oscillating is whether the purifier turns from side to side.
	Oscillating bool
	// NightMode is the quiet, dimmed mode.
	NightMode bool
	// Monitoring is whether the sensors go on reading with the fan off.
	Monitoring bool
	// FilterLife is the hours of use the filter has left.
	FilterLife time.Duration
	// Error and Warning are the purifier's codes; empty when it has none.
	Error   string
	Warning string
}

// Environment is what a purifier's sensors read. A reading the purifier
// has none of, a sensor that is off, starting or failed, is nil.
type Environment struct {
	// Temperature is in Celsius, Humidity in percent.
	Temperature *float64
	Humidity    *int
	// Particulates and VOC are the purifier's own indices, 0 the cleanest;
	// the reference implementation gives them no unit.
	Particulates *int
	VOC          *int
	// SleepTimer is what is left of the sleep timer; zero when none runs.
	SleepTimer time.Duration
}

// The words a purifier uses for a setting.
const (
	wordOn   = "ON"
	wordOff  = "OFF"
	wordNone = "NONE"
)

// field is one field of a report as text: itself, or, in a change, the
// second of its two values, the one that holds now. Absent, it is empty.
func field(fields map[string]json.RawMessage, name string) string {
	var (
		value string
		pair  []string
	)

	raw := fields[name]

	switch {
	case json.Unmarshal(raw, &value) == nil:
		return value
	case json.Unmarshal(raw, &pair) == nil && len(pair) > 1:
		return pair[1]
	default:
		return ""
	}
}

// state reads a state. A report with no mode in it is none.
func (r *report) state() (State, error) {
	mode := Mode(field(r.Product, "fmod"))
	if mode != Off && mode != Fan && mode != Auto {
		return State{}, fmt.Errorf("%w: a state whose mode is %q", ErrAnswer, mode)
	}

	state := State{
		Mode:        mode,
		Running:     field(r.Product, "fnst") == string(Fan),
		Oscillating: field(r.Product, "oson") == wordOn,
		NightMode:   field(r.Product, "nmod") == wordOn,
		Monitoring:  field(r.Product, "rhtm") == wordOn,
	}

	// The speed is a number, or AUTO; the filter's life a number of hours.
	state.Speed, _ = strconv.Atoi(field(r.Product, "fnsp"))

	if hours, err := strconv.Atoi(field(r.Product, "filf")); err == nil {
		state.FilterLife = time.Duration(hours) * time.Hour
	}

	if code := field(r.Product, "ercd"); code != wordNone {
		state.Error = code
	}

	if code := field(r.Product, "wacd"); code != wordNone {
		state.Warning = code
	}

	return state, nil
}

// reading is a sensor's field as a number; nil when the purifier wrote
// OFF, INIT, FAIL or nothing there.
func reading(fields map[string]json.RawMessage, name string) *int {
	value, err := strconv.Atoi(field(fields, name))
	if err != nil {
		return nil
	}

	return &value
}

// environment reads the sensors.
func (r *report) environment() Environment {
	environment := Environment{
		Humidity:     reading(r.Data, "hact"),
		Particulates: reading(r.Data, "pact"),
		VOC:          reading(r.Data, "vact"),
	}

	// The temperature is written in tenths of a kelvin.
	if tenths := reading(r.Data, "tact"); tenths != nil {
		const perKelvin, freezing = 10.0, 273.15

		celsius := float64(*tenths)/perKelvin - freezing
		environment.Temperature = &celsius
	}

	if minutes := reading(r.Data, "sltm"); minutes != nil {
		environment.SleepTimer = time.Duration(*minutes) * time.Minute
	}

	return environment
}

// word is a setting as the purifier writes it.
func word(on bool) string {
	if on {
		return wordOn
	}

	return wordOff
}

// fourDigits is a number as the purifier writes one.
func fourDigits(value int) string { return fmt.Sprintf("%04d", value) }

// SetMode turns the fan off, on at the speed set, or over to the
// purifier. Like every Set, it returns when the purifier's broker has
// taken the request; the purifier says what it then does, which Next
// reads.
func (p *Purifier) SetMode(ctx context.Context, mode Mode) error {
	if mode != Off && mode != Fan && mode != Auto {
		return fmt.Errorf("%w: the mode %q", ErrArgument, mode)
	}

	return p.send(ctx, setState, map[string]string{"fmod": string(mode)})
}

// SetSpeed turns the fan on at a speed, MinSpeed to MaxSpeed.
func (p *Purifier) SetSpeed(ctx context.Context, speed int) error {
	if speed < MinSpeed || speed > MaxSpeed {
		return fmt.Errorf("%w: a speed of %d, outside %d to %d", ErrArgument, speed, MinSpeed, MaxSpeed)
	}

	return p.send(ctx, setState, map[string]string{"fmod": string(Fan), "fnsp": fourDigits(speed)})
}

// SetOscillation has the purifier turn from side to side, or stop.
func (p *Purifier) SetOscillation(ctx context.Context, on bool) error {
	return p.send(ctx, setState, map[string]string{"oson": word(on)})
}

// SetNightMode sets the quiet, dimmed mode, or ends it.
func (p *Purifier) SetNightMode(ctx context.Context, on bool) error {
	return p.send(ctx, setState, map[string]string{"nmod": word(on)})
}

// SetSleepTimer has the purifier turn itself off after a time, a minute
// to MaxSleepTimer, in whole minutes; zero ends the timer.
func (p *Purifier) SetSleepTimer(ctx context.Context, after time.Duration) error {
	if after == 0 {
		return p.send(ctx, setState, map[string]string{"sltm": wordOff})
	}

	if after < time.Minute || after > MaxSleepTimer {
		return fmt.Errorf("%w: a sleep timer of %s, outside %s to %s", ErrArgument, after, time.Minute, MaxSleepTimer)
	}

	return p.send(ctx, setState, map[string]string{"sltm": fourDigits(int(after / time.Minute))})
}

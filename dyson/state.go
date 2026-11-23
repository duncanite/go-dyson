package dyson

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Mode is what the fan is set to do.
type Mode string

// The modes of a purifier.
const (
	// Off is the fan off; the sensors may go on reading.
	Off Mode = "OFF"
	// Fan is the fan on, at the speed set.
	Fan Mode = "FAN"
	// Auto is the fan run by the purifier, on when the air needs it.
	Auto Mode = "AUTO"
)

// The fan's speeds, the longest sleep timer, and the temperatures a
// heater is set to, in Celsius as Heat.Target gives them: 274 and 310
// kelvin.
const (
	MinSpeed      = 1
	MaxSpeed      = 10
	MaxSleepTimer = 540 * time.Minute
	MinHeatTarget = 0.8
	MaxHeatTarget = 36.8
)

// TargetHeld is how far above Heat.Target the temperature a purifier
// holds is, in Celsius. A purifier holds a target in tenths of a kelvin,
// which fall halfway between two tenths of a degree Celsius: Target is the
// tenth below, so that a target set is the target read back.
const TargetHeld = 0.05

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
	// FilterLife is the hours of use the filter of a Link has left; zero
	// for the generation after it, which has two filters and says how
	// much of each is left.
	FilterLife time.Duration
	// HEPA and Carbon are how much of each filter of the generation after
	// the Link's is left, in percent; nil for a Link, and for a filter the
	// purifier says nothing of.
	HEPA   *int
	Carbon *int
	// Heat is the heater of a Hot+Cool; nil for a purifier with none.
	Heat *Heat
	// Error and Warning are the purifier's codes; empty when it has none.
	Error   string
	Warning string
}

// Heat is the heater of a Hot+Cool purifier.
type Heat struct {
	// On is whether the purifier is set to heat, and Heating whether it
	// heats now: set to heat, it stops when the room is at its target.
	On      bool
	Heating bool
	// Target is the temperature it heats to, in Celsius, to the tenth of
	// a degree: what SetHeatTarget was given, when it was given a tenth.
	// The purifier holds TargetHeld above it. Nil when the purifier did
	// not say.
	Target *float64
}

// Environment is what a purifier's sensors read. A reading the purifier
// has none of, a sensor that is off, starting or failed, is nil.
type Environment struct {
	// Temperature is in Celsius, Humidity in percent.
	Temperature *float64
	Humidity    *int
	// Particulates and VOC are a Link's own indices, 0 the cleanest; the
	// reference implementation gives them no unit. Nil for the generation
	// after it.
	Particulates *int
	VOC          *int
	// PM25 and PM10 are the particulates of the generation after the
	// Link's, in micrograms per cubic metre; VOCIndex and NO2Index its
	// indices of volatile compounds and of nitrogen dioxide. Nil for a
	// Link.
	PM25     *int
	PM10     *int
	VOCIndex *float64
	NO2Index *float64
	// SleepTimer is what is left of the sleep timer; zero when none runs.
	SleepTimer time.Duration
}

// The fields of a state that are set as well as read.
const (
	fieldPower  = "fpwr"
	fieldSwing  = "oson"
	fieldHeater = "hmod"
)

// The words a purifier uses for a setting.
const (
	wordOn   = "ON"
	wordOff  = "OFF"
	wordNone = "NONE"
	wordHeat = "HEAT"
	// The generation after the Link's writes its oscillation as ON and OFF
	// or as these two, by its firmware.
	wordSwinging = "OION"
	wordStill    = "OIOF"
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

// state reads a state, as the purifier's generation writes one.
func (r *report) state(speaks generation) (State, error) {
	var state State

	if speaks == link {
		state.Mode = Mode(field(r.Product, "fmod"))
		if state.Mode != Off && state.Mode != Fan && state.Mode != Auto {
			return State{}, fmt.Errorf("%w: a state whose mode is %q", ErrAnswer, state.Mode)
		}

		state.Oscillating = field(r.Product, fieldSwing) == wordOn

		// The filter's life is a number of hours.
		if hours, err := strconv.Atoi(field(r.Product, "filf")); err == nil {
			state.FilterLife = time.Duration(hours) * time.Hour
		}
	} else {
		power := field(r.Product, fieldPower)

		switch {
		case power == wordOff:
			state.Mode = Off
		case power != wordOn:
			return State{}, fmt.Errorf("%w: a state whose power is %q", ErrAnswer, power)
		case field(r.Product, "auto") == wordOn:
			state.Mode = Auto
		default:
			state.Mode = Fan
		}

		swing := field(r.Product, fieldSwing)
		state.Oscillating = swing == wordOn || swing == wordSwinging

		// Each filter's life is a percentage; a filter that is not there
		// is INV.
		state.HEPA, state.Carbon = reading(r.Product, "hflr"), reading(r.Product, "cflr")
		state.Heat = r.heat()
	}

	state.Running = field(r.Product, "fnst") == string(Fan)
	state.NightMode = field(r.Product, "nmod") == wordOn
	state.Monitoring = field(r.Product, "rhtm") == wordOn

	// The speed is a number, or AUTO.
	state.Speed, _ = strconv.Atoi(field(r.Product, "fnsp"))

	if code := field(r.Product, "ercd"); code != wordNone {
		state.Error = code
	}

	if code := field(r.Product, "wacd"); code != wordNone {
		state.Warning = code
	}

	return state, nil
}

// heat reads the heater; nil when the state says nothing of one.
func (r *report) heat() *Heat {
	mode := field(r.Product, fieldHeater)
	if mode == "" {
		return nil
	}

	heat := &Heat{On: mode == wordHeat, Heating: field(r.Product, "hsta") == wordHeat}

	if tenths := reading(r.Product, "hmax"); tenths != nil {
		target := float64(*tenths-aboveFreezing) / perUnit
		heat.Target = &target
	}

	return heat
}

// What is written in tenths: the temperatures, in kelvin, and the indices
// of the generation after the Link's.
const (
	perUnit  = 10.0
	freezing = 273.15
	// aboveFreezing is the tenth of a kelvin a heat target of zero
	// Celsius is written as: 273.15 kelvin, taken up to the tenth.
	aboveFreezing = 2732
)

func celsius(tenths int) float64 { return float64(tenths)/perUnit - freezing }

// reading is a field as a number; nil when the purifier wrote OFF, INIT,
// FAIL, INV or nothing there.
func reading(fields map[string]json.RawMessage, name string) *int {
	value, err := strconv.Atoi(field(fields, name))
	if err != nil {
		return nil
	}

	return &value
}

// tenth is a field written in tenths, as a number; nil as reading is.
func tenth(fields map[string]json.RawMessage, name string) *float64 {
	tenths := reading(fields, name)
	if tenths == nil {
		return nil
	}

	value := float64(*tenths) / perUnit

	return &value
}

// environment reads the sensors, as the purifier's generation names them.
func (r *report) environment(speaks generation) Environment {
	environment := Environment{Humidity: reading(r.Data, "hact")}

	if speaks == link {
		environment.Particulates, environment.VOC = reading(r.Data, "pact"), reading(r.Data, "vact")
	} else {
		// A particulate reading is under one name or the other, by the
		// firmware; the reference implementation takes the first it finds.
		if environment.PM25 = reading(r.Data, "p25r"); environment.PM25 == nil {
			environment.PM25 = reading(r.Data, "pm25")
		}

		if environment.PM10 = reading(r.Data, "p10r"); environment.PM10 == nil {
			environment.PM10 = reading(r.Data, "pm10")
		}

		environment.VOCIndex, environment.NO2Index = tenth(r.Data, "va10"), tenth(r.Data, "noxl")
	}

	if tenths := reading(r.Data, "tact"); tenths != nil {
		celsius := celsius(*tenths)
		environment.Temperature = &celsius
	}

	if minutes := reading(r.Data, "sltm"); minutes != nil {
		environment.SleepTimer = time.Duration(*minutes) * time.Minute
	}

	return environment
}

// remember keeps how a state wrote the oscillation and its angles, for
// SetOscillation to say back.
func (p *Purifier) remember(heard *report) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Whatever the state holds: one that writes no oscillation has said
	// all there is to learn, and the purifier is told in the usual words.
	if !p.knows {
		p.knows = true

		close(p.told)
	}

	if swing := field(heard.Product, fieldSwing); swing != "" {
		p.swing = swing
	}

	if preset := field(heard.Product, "ancp"); preset != "" {
		p.preset = preset
	}

	if low := field(heard.Product, "osal"); low != "" {
		p.low = low
	}

	if high := field(heard.Product, "osau"); high != "" {
		p.high = high
	}
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

	if p.speaks == link {
		return p.send(ctx, setState, map[string]string{"fmod": string(mode)})
	}

	// The generation after the Link's has a power and an auto mode, where
	// the Link has one mode. Auto is sent with the power on: the reference
	// implementation sends the auto mode alone, and what a purifier that
	// is off then does was not read.
	return p.send(ctx, setState, map[Mode]map[string]string{
		Off:  {fieldPower: wordOff},
		Fan:  {fieldPower: wordOn, "auto": wordOff},
		Auto: {fieldPower: wordOn, "auto": wordOn},
	}[mode])
}

// SetSpeed turns the fan on at a speed, MinSpeed to MaxSpeed.
func (p *Purifier) SetSpeed(ctx context.Context, speed int) error {
	if speed < MinSpeed || speed > MaxSpeed {
		return fmt.Errorf("%w: a speed of %d, outside %d to %d", ErrArgument, speed, MinSpeed, MaxSpeed)
	}

	if p.speaks == link {
		return p.send(ctx, setState, map[string]string{"fmod": string(Fan), "fnsp": fourDigits(speed)})
	}

	return p.send(ctx, setState, map[string]string{fieldPower: wordOn, "fnsp": fourDigits(speed)})
}

// SetOscillation has the purifier turn from side to side, or stop.
//
// The generation after the Link's is told to turn in the words it last
// wrote its oscillation in, with the sweep and the angles it last said it
// turns between, and its fan on. When it has said no state on this
// connection yet, SetOscillation first asks for one and waits for it: read
// by the goroutine that is in Next when there is one, and by itself
// otherwise, in which case what it read is Next's to give all the same.
func (p *Purifier) SetOscillation(ctx context.Context, turning bool) error {
	if p.speaks == link {
		return p.send(ctx, setState, map[string]string{fieldSwing: word(turning)})
	}

	if err := p.learn(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	swing, preset, low, high := p.swing, p.preset, p.low, p.high
	p.mu.Unlock()

	swinging, still := wordOn, wordOff
	if swing == wordSwinging || swing == wordStill {
		swinging, still = wordSwinging, wordStill
	}

	if !turning {
		return p.send(ctx, setState, map[string]string{fieldSwing: still})
	}

	turn := map[string]string{fieldSwing: swinging, fieldPower: wordOn}

	// The sweep is said back as the purifier last said it, so that turning
	// changes nothing of how it turns; one that said none is given its
	// angles as its own, as the reference implementation does.
	if low != "" && high != "" {
		turn["ancp"], turn["osal"], turn["osau"] = "CUST", low, high

		if preset != "" {
			turn["ancp"] = preset
		}
	}

	return p.send(ctx, setState, turn)
}

// learn returns once a state has been read, which says how the purifier
// writes its oscillation, asking for one when none has. Whoever is in Next
// reads it; with nobody there, learn reads, and keeps what it read for
// Next.
func (p *Purifier) learn(ctx context.Context) error {
	select {
	case <-p.told:
		return nil
	default:
	}

	if err := p.send(ctx, askState, nil); err != nil {
		return err
	}

	for {
		select {
		case <-p.told:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("the purifier: waiting for its state: %w", context.Cause(ctx))
		case p.reading <- struct{}{}:
		}

		// The state may have come as the turn to read was taken: there
		// is then nothing to wait for.
		select {
		case <-p.told:
			<-p.reading

			return nil
		default:
		}

		update, err := p.read(ctx)

		// What was read is kept; a wait that ended with nothing read is
		// this call's alone.
		if err == nil || update.Whole != nil {
			p.mu.Lock()
			p.kept = append(p.kept, saying{update: update, err: err})
			p.mu.Unlock()
		}

		<-p.reading

		if err != nil && update.Whole == nil {
			return err
		}
	}
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

// SetHeat sets a Hot+Cool to heat, to the target it has, or to heat no
// more. A purifier with no heater is ErrUnsupported, and nothing is sent.
func (p *Purifier) SetHeat(ctx context.Context, on bool) error {
	if p.speaks != hotCool {
		return fmt.Errorf("%w: it has no heater", ErrUnsupported)
	}

	if !on {
		return p.send(ctx, setState, map[string]string{fieldHeater: wordOff})
	}

	return p.send(ctx, setState, map[string]string{fieldHeater: wordHeat})
}

// SetHeatTarget sets a Hot+Cool to heat to a temperature in Celsius,
// MinHeatTarget to MaxHeatTarget. A tenth of a degree is read back as it
// was set, in Heat.Target; anything finer is sent as the tenth of a
// kelvin nearest to it, which is how a purifier set in Fahrenheit holds a
// whole degree of its own. A purifier with no heater is ErrUnsupported,
// and nothing is sent.
func (p *Purifier) SetHeatTarget(ctx context.Context, celsius float64) error {
	if p.speaks != hotCool {
		return fmt.Errorf("%w: it has no heater", ErrUnsupported)
	}

	// The bounds are held in the unit sent, tenths of a kelvin: a target at
	// either end is not lost to a float's last digit. A NaN is outside
	// every range, and compares as inside none.
	const lowest, highest = 2740, 3100

	// The nearest tenth of a kelvin, a tie going up: a tenth of a degree
	// Celsius is always a tie, and is sent as the tenth of a kelvin that
	// reads back as itself. The slack keeps what arithmetic left a hair
	// under a tenth from being sent a tenth down.
	const slack = 1e-6

	tenths := math.Floor(celsius*perUnit + aboveFreezing + slack)
	if !(tenths >= lowest && tenths <= highest) {
		return fmt.Errorf("%w: a heat target of %g, outside %g to %g Celsius",
			ErrArgument, celsius, MinHeatTarget, MaxHeatTarget)
	}

	return p.send(ctx, setState, map[string]string{fieldHeater: wordHeat, "hmax": fourDigits(int(tenths))})
}

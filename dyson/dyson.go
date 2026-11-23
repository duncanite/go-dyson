// Package dyson speaks to a Dyson Pure Cool Link purifier, the TP02, on
// the LAN: the purifier runs an MQTT broker, and with the credential on
// its Wi-Fi sticker a client reads its state and its sensors there, and
// drives it. No account and no cloud.
//
// Dyson documents none of this. The contract is libdyson-neon, the
// reference implementation the open projects share; docs/design.md says
// what was read there. Only the Link generation's product type 475 is
// spoken: later generations name their state otherwise.
//
// A Purifier is one connection, and its methods may be called from several
// goroutines: one waits in Next for what the purifier says while others
// set it. What the purifier says arrives on one subscription, which Next,
// State and Environment all read, one of them at a time.
package dyson

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/duncanite/go-dyson/mqtt"
)

// Port is the port a purifier's broker listens on.
const Port = "1883"

// The product types this package speaks.
const (
	// PureCoolLink is the TP02.
	PureCoolLink = "475"
	// PureHotCool is the HP04, of the generation after the Link's, and
	// with a letter after it the HP07 and the HP09, which speak as it does.
	PureHotCool = "527"
)

// generation is how a product type speaks: the names of its state's
// fields and of its sensors', and whether it heats.
type generation uint8

const (
	// link is the Link generation: the type 475.
	link generation = iota + 1
	// hotCool is the generation after it, with a heater: the type 527 and
	// its variants.
	hotCool
)

// generationOf is how a product type speaks, when this package speaks it.
func generationOf(product string) (generation, bool) {
	switch product {
	case PureCoolLink:
		return link, true
	case PureHotCool, PureHotCool + "E", PureHotCool + "K", PureHotCool + "M":
		return hotCool, true
	default:
		return 0, false
	}
}

// Speaks says whether a product type is one this package speaks: a type
// Dial takes, where it refuses any other as ErrUnsupported. It is what a
// purifier announces and what its sticker carries.
func Speaks(product string) bool {
	_, spoken := generationOf(product)

	return spoken
}

// The errors, to test with errors.Is. What the connection itself refuses
// is package mqtt's: mqtt.ErrCredentials for a wrong credential,
// mqtt.ErrLost when the purifier goes.
var (
	// ErrSticker: the network name is not a Dyson sticker's.
	ErrSticker = errors.New("not a Dyson sticker's network name")
	// ErrUnsupported: a product type this package does not speak.
	ErrUnsupported = errors.New("not a product type this package speaks")
	// ErrAnswer: what the purifier said is not in the shape the reference
	// implementation reads.
	ErrAnswer = errors.New("the purifier's answer is not in the expected shape")
	// ErrArgument: a value the purifier has no meaning for; nothing was
	// sent.
	ErrArgument = errors.New("not a valid argument")
)

// Identity is what names a purifier and opens it. The credential is a
// secret: an Identity formatted, logged or marshalled as JSON shows its
// serial and its type, and not that. One kept in an unexported field of
// another struct is out of reach of these methods, and fmt prints it as it
// is.
type Identity struct {
	// Serial is the purifier's serial number, the user name of its broker.
	Serial string
	// Type is its product type, the first level of its topics.
	Type string
	// Credential is the broker's password.
	Credential string
}

// Format keeps the credential out of a formatted value, whatever the
// verb.
func (i Identity) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, i.shown()) }

// LogValue keeps it out of a log record.
func (i Identity) LogValue() slog.Value { return slog.StringValue(i.shown()) }

// MarshalJSON keeps it out of JSON: an Identity marshals as what it
// shows, a string.
func (i Identity) MarshalJSON() ([]byte, error) {
	shown, err := json.Marshal(i.shown())
	if err != nil {
		return nil, fmt.Errorf("marshalling dyson.Identity: %w", err)
	}

	return shown, nil
}

// shown is an Identity without its credential.
func (i Identity) shown() string {
	return fmt.Sprintf("dyson.Identity{Serial:%q Type:%q}", i.Serial, i.Type)
}

// sticker is a purifier's own network name: DYSON, the serial, the product
// type.
var sticker = regexp.MustCompile(`^DYSON-([0-9A-Z]{3}-[A-Z]{2}-[0-9A-Z]{8,})-(\d{3}[A-Z]?)$`)

// FromSticker is the identity of the purifier whose Wi-Fi sticker carries
// this network name and this password: the serial and the product type
// are in the name, and the credential is the base64 of the password's
// SHA-512.
func FromSticker(ssid, password string) (Identity, error) {
	parts := sticker.FindStringSubmatch(ssid)
	if parts == nil {
		// What was given is not shown: it may be the password, put where
		// the name goes.
		return Identity{}, fmt.Errorf("%w: it reads DYSON-<serial>-<type>", ErrSticker)
	}

	if password == "" {
		return Identity{}, fmt.Errorf("%w: an empty Wi-Fi password", ErrArgument)
	}

	digest := sha512.Sum512([]byte(password))

	// One sticker names a type its topics do not use.
	product := parts[2]
	if product == "455A" {
		product = "455"
	}

	return Identity{
		Serial:     parts[1],
		Type:       product,
		Credential: base64.StdEncoding.EncodeToString(digest[:]),
	}, nil
}

// Purifier is a connection to one purifier. Its methods may be called
// from several goroutines.
//
// What the purifier says is one stream, and each thing said goes to one
// reader: a goroutine in Next takes the answers that State and
// Environment, called from another, wait for. A consumer that watches
// calls Ask and reads the two answers from Next.
type Purifier struct {
	conn    *mqtt.Conn
	said    *mqtt.Subscription
	command string
	speaks  generation

	// reading is held by the one goroutine that reads what the purifier
	// says. It is a channel so that a wait for it ends with a context.
	reading chan struct{}

	mu sync.Mutex
	// swing is how the purifier last wrote its oscillation, preset the
	// sweep it said it was set to, and low and high the angles it turns
	// between: what the generation after the Link's has to be told back
	// to turn. Empty until a state was read.
	swing, preset, low, high string
	// told is closed once a state was read: it said how the purifier
	// writes its oscillation, or that it writes none.
	told  chan struct{}
	knows bool
	// kept is what SetOscillation read while it waited for a state, and
	// Next has not given yet.
	kept []saying
}

// saying is one thing read of what the purifier says: what Next returns.
type saying struct {
	update Update
	err    error
}

// Dial connects to the purifier at host, an address with or without a
// port, and subscribes to what it says. The purifier's broker counts its
// connections and some models run out of them: Close the Purifier.
func Dial(ctx context.Context, host string, identity Identity) (*Purifier, error) {
	speaks, spoken := generationOf(identity.Type)
	if !spoken {
		return nil, fmt.Errorf("%w: %q; it speaks %s, the Pure Cool Link, and %s, the Pure Hot+Cool",
			ErrUnsupported, identity.Type, PureCoolLink, PureHotCool)
	}

	if host == "" || identity.Serial == "" || identity.Credential == "" {
		return nil, fmt.Errorf("%w: a host, a serial and a credential are all needed", ErrArgument)
	}

	address := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		address = net.JoinHostPort(host, Port)
	}

	conn, err := mqtt.Dial(ctx, mqtt.Options{
		Address:  address,
		Version:  mqtt.V31,
		Username: identity.Serial,
		Password: identity.Credential,
	})
	if err != nil {
		return nil, fmt.Errorf("the purifier %s: %w", identity.Serial, err)
	}

	topics := identity.Type + "/" + identity.Serial

	said, err := conn.Subscribe(ctx, topics+"/status/current", mqtt.AtMostOnce)
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("the purifier %s: %w", identity.Serial, err)
	}

	return &Purifier{
		conn: conn, said: said, command: topics + "/command", speaks: speaks,
		reading: make(chan struct{}, 1), told: make(chan struct{}),
	}, nil
}

// Close ends the connection.
func (p *Purifier) Close() error {
	return p.conn.Close()
}

// Done is closed when the connection is over: closed, or lost. It is what a
// caller waits on beside its other work to learn that the purifier went,
// with nothing asked of it. Nothing connects again by itself: a purifier
// that went is the caller's to Dial again.
func (p *Purifier) Done() <-chan struct{} {
	return p.conn.Done()
}

// Err is nil while the connection lives, mqtt.ErrClosed after Close, and
// an mqtt.ErrLost with its cause when the purifier went by itself.
func (p *Purifier) Err() error {
	return p.conn.Err()
}

// The messages, by their msg.
const (
	askState       = "REQUEST-CURRENT-STATE"
	askEnvironment = "REQUEST-PRODUCT-ENVIRONMENT-CURRENT-SENSOR-DATA"
	setState       = "STATE-SET"
	currentState   = "CURRENT-STATE"
	stateChange    = "STATE-CHANGE"
	sensorData     = "ENVIRONMENTAL-CURRENT-SENSOR-DATA"
)

// request is a message to the purifier; report is one from it.
type (
	request struct {
		Message string `json:"msg"`
		Time    string `json:"time"`
		// Reason and Data are a STATE-SET's: who asks, and what for.
		Reason string            `json:"mode-reason,omitempty"`
		Data   map[string]string `json:"data,omitempty"`
	}
	report struct {
		Message string `json:"msg"`
		// Product is a state's fields: each a value, or, in a change, the
		// value before and the value now.
		Product map[string]json.RawMessage `json:"product-state"`
		// Data is the sensors' fields.
		Data map[string]json.RawMessage `json:"data"`
	}
)

// Update is something the purifier said: its state, or what its sensors
// read. One of the two is set.
type Update struct {
	State       *State
	Environment *Environment
	// Whole is the message as the purifier sent it, in JSON: what this
	// package read the other two from, and whatever else the purifier put
	// there. It is the purifier's own text, and is shown as such.
	//
	// It is set too on the Update that comes with an ErrAnswer, where
	// neither of the other two is: the message this package could not
	// read, which may then be no JSON.
	Whole json.RawMessage
}

// Next is the next thing the purifier says, waiting for it until ctx is
// done: a change of its state, which it sends as it happens, or its
// sensors' readings, which it sends when asked and from time to time. A
// message of another kind is passed over. One that is not in the shape the
// reference implementation reads is ErrAnswer, and comes whole with it.
//
// A Next that ends with ctx has taken nothing: the next call gives what
// it would have. Called from several goroutines, each thing said goes to
// one of them.
func (p *Purifier) Next(ctx context.Context) (Update, error) {
	select {
	case p.reading <- struct{}{}:
		defer func() { <-p.reading }()
	case <-ctx.Done():
		return Update{}, fmt.Errorf("the purifier: waiting to read: %w", context.Cause(ctx))
	}

	p.mu.Lock()

	if len(p.kept) > 0 {
		first := p.kept[0]
		p.kept = p.kept[1:]
		p.mu.Unlock()

		return first.update, first.err
	}

	p.mu.Unlock()

	return p.read(ctx)
}

// Ask has the purifier say its state and what its sensors read, and waits
// for neither: Next reads the two answers, among whatever else the
// purifier says.
func (p *Purifier) Ask(ctx context.Context) error {
	if err := p.send(ctx, askState, nil); err != nil {
		return err
	}

	return p.send(ctx, askEnvironment, nil)
}

// State asks the purifier for its state and waits for it. What the
// purifier says of its sensors meanwhile is passed over.
func (p *Purifier) State(ctx context.Context) (State, error) {
	if err := p.send(ctx, askState, nil); err != nil {
		return State{}, err
	}

	for {
		update, err := p.Next(ctx)
		if err != nil {
			return State{}, err
		}

		if update.State != nil {
			return *update.State, nil
		}
	}
}

// Environment asks the purifier for what its sensors read and waits for
// it. A change of state it says meanwhile is passed over.
func (p *Purifier) Environment(ctx context.Context) (Environment, error) {
	if err := p.send(ctx, askEnvironment, nil); err != nil {
		return Environment{}, err
	}

	for {
		update, err := p.Next(ctx)
		if err != nil {
			return Environment{}, err
		}

		if update.Environment != nil {
			return *update.Environment, nil
		}
	}
}

// read takes the next thing the purifier says off the subscription. The
// caller holds reading.
func (p *Purifier) read(ctx context.Context) (Update, error) {
	for {
		message, err := p.said.Next(ctx)
		if err != nil {
			return Update{}, fmt.Errorf("the purifier: %w", err)
		}

		var heard report
		if err = json.Unmarshal(message.Payload, &heard); err != nil {
			return Update{Whole: message.Payload}, fmt.Errorf("%w: %w", ErrAnswer, err)
		}

		switch heard.Message {
		case currentState, stateChange:
			state, err := heard.state(p.speaks)
			if err != nil {
				return Update{Whole: message.Payload}, err
			}

			p.remember(&heard)

			return Update{State: &state, Whole: message.Payload}, nil
		case sensorData:
			environment := heard.environment(p.speaks)

			return Update{Environment: &environment, Whole: message.Payload}, nil
		}
	}
}

// send publishes one message to the purifier.
func (p *Purifier) send(ctx context.Context, message string, data map[string]string) error {
	asked := request{Message: message, Time: time.Now().UTC().Format("2006-01-02T15:04:05Z"), Data: data}

	// A change is sent as the app's own, and acknowledged; a question is
	// not.
	qos := mqtt.AtMostOnce
	if data != nil {
		asked.Reason, qos = "LAPP", mqtt.AtLeastOnce
	}

	payload, err := json.Marshal(asked)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrArgument, message, err)
	}

	if err = p.conn.Publish(ctx, mqtt.Message{Topic: p.command, Payload: payload, QoS: qos}); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}

	return nil
}

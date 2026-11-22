// Package dysontest is a purifier for tests, a Pure Cool Link or a Pure
// Hot+Cool: an MQTT broker that takes the session a purifier's own broker
// takes, MQTT 3.1 under the serial and the credential, and answers on the
// purifier's topics as the reference implementation, libdyson-neon, reads
// them: its state when asked, its sensors when asked, and a change of
// state for each one set. A test reads and sets its state, and sees every
// message it was sent.
//
// What a purifier does that the reference implementation does not show is
// the fake's own choice: the fan it reports as turning is the one set to
// FAN, or powered; a heater set to heat is one that heats; and it answers
// a message it does not know with nothing.
package dysontest

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"maps"
	"sync"

	"github.com/duncanite/go-dyson/mqtt"
	"github.com/duncanite/go-dyson/mqtt/mqtttest"
)

// The purifier's sticker, and what it makes of it.
const (
	Serial       = "NN2-US-HKA0000A"
	SSID         = "DYSON-" + Serial + "-475"
	WiFiPassword = "sticker-pass" // #nosec G101 -- the fake's own password, of no purifier
	Status       = "475/" + Serial + "/status/current"
	Command      = "475/" + Serial + "/command"
)

// The Hot+Cool's sticker, and what it makes of it: a purifier of the
// generation after the Link's, of type 527.
const (
	HotCoolSerial       = "ZZ9-US-HKA0000B"
	HotCoolSSID         = "DYSON-" + HotCoolSerial + "-527"
	HotCoolWiFiPassword = "hot-cool-pass" // #nosec G101 -- the fake's own password, of no purifier
)

// The words of a purifier's state the fake itself writes, and the field
// its sleep timer is in.
const (
	fan       = "FAN"
	off       = "OFF"
	powered   = "ON"
	heat      = "HEAT"
	none      = "NONE"
	fieldTime = "sltm"
)

// Received is one message the purifier was sent.
type Received struct {
	Message string            `json:"msg"`
	Time    string            `json:"time"`
	Reason  string            `json:"mode-reason"`
	Data    map[string]string `json:"data"`
	// QoS is the quality of service it came with.
	QoS mqtt.QoS `json:"-"`
}

// Purifier is the fake. A test changes Product and Sensors through Set,
// or before a client connects.
type Purifier struct {
	*mqtttest.Broker

	mu sync.Mutex
	// Product is the state's fields, Sensors the sensors', as the purifier
	// writes them.
	Product map[string]string
	Sensors map[string]string

	received []Received
	// status and command are its topics; hotCool is whether it is of the
	// generation after the Link's.
	status  string
	command string
	hotCool bool
}

// New starts a purifier: its fan on at speed 4, oscillating, a filter
// with 2,159 hours left, the air at 297.7 kelvin. The caller closes it.
func New() *Purifier {
	purifier := &Purifier{
		Broker: mqtttest.New(),
		Product: map[string]string{
			"fmod": fan, "fnst": fan, "fnsp": "0004", "qtar": "0004", "oson": powered, "rhtm": powered,
			"filf": "2159", "ercd": none, "nmod": off, "wacd": none,
		},
		Sensors: map[string]string{"tact": "2977", "hact": "0058", "pact": "0000", "vact": "0003", fieldTime: off},
		status:  Status,
		command: Command,
	}

	purifier.serve(Serial, WiFiPassword)

	return purifier
}

// NewHotCool starts a Pure Hot+Cool, of type 527: its fan on at speed 5
// and turning between 63 and 243 degrees, written as the newer firmware
// writes it, its filters at 95 and 80 percent, its heater off with a
// target of 294 kelvin, the air at 295 kelvin. The caller closes it.
func NewHotCool() *Purifier { return NewHotCoolVariant("") }

// NewHotCoolVariant starts a Pure Hot+Cool of a variant of the type 527,
// named by its letter, E, K or M: the same purifier, on that type's
// topics. Its sticker's network name ends in the type, 527 and the
// letter. The caller closes it.
func NewHotCoolVariant(letter string) *Purifier {
	topics := "527" + letter + "/" + HotCoolSerial

	purifier := &Purifier{
		Broker: mqtttest.New(),
		Product: map[string]string{
			"fpwr": powered,
			"fdir": powered,
			"auto": off,
			"oscs": powered,
			"oson": "OION",
			"nmod": off,
			"rhtm": powered,
			"fnst": fan,
			"ercd": none,
			"wacd": none,
			"nmdv": "0004",
			"fnsp": "0005",
			"bril": "0002",
			"corf": powered,
			"cflr": "0080",
			"hflr": "0095",
			"osal": "0063",
			"osau": "0243",
			"ancp": "CUST",
			"hmod": off,
			"hmax": "2940",
			"hsta": off,
			"ffoc": off,
			"tilt": "OK",
		},
		Sensors: map[string]string{
			"tact": "2950", "hact": "0045", "pm25": "0006", "pm10": "0009", "va10": "0004", "noxl": "0012",
			"p25r": "0007", "p10r": "0010", fieldTime: off,
		},
		status:  topics + "/status/current",
		command: topics + "/command",
		hotCool: true,
	}

	purifier.serve(HotCoolSerial, HotCoolWiFiPassword)

	return purifier
}

// Received is the messages the purifier was sent, in order.
func (p *Purifier) Received() []Received {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]Received(nil), p.received...)
}

// Set changes one field of the state, as a hand on the purifier's remote
// would, and has the purifier say so.
func (p *Purifier) Set(name, value string) {
	p.mu.Lock()
	change := p.change(map[string]string{name: value})
	p.mu.Unlock()

	p.Say("STATE-CHANGE", "product-state", change)
}

// Say has the purifier publish a message of a kind, with fields under a
// name.
func (p *Purifier) Say(kind, name string, fields any) {
	payload, err := json.Marshal(map[string]any{"msg": kind, "time": "2026-01-01T00:00:00.000Z", name: fields})
	if err != nil {
		panic(err)
	}

	p.Publish(mqtt.Message{Topic: p.status, Payload: payload})
}

// serve has the broker take the purifier's own session and no other, and
// answer on its topics.
func (p *Purifier) serve(serial, password string) {
	digest := sha512.Sum512([]byte(password))
	credential := base64.StdEncoding.EncodeToString(digest[:])

	p.Accept = func(hello mqtttest.Hello) mqtttest.Code {
		switch {
		case hello.Protocol != "MQIsdp" || hello.Version != 3:
			return mqtttest.BadVersion
		case hello.Username != serial || hello.Password != credential:
			return mqtttest.BadCredentials
		default:
			return mqtttest.Accepted
		}
	}
	p.Handle = p.handle
}

// change sets fields of the state and gives each field of the state as
// the purifier writes a change: the value before, the value now. The lock
// is held.
func (p *Purifier) change(set map[string]string) map[string][2]string {
	before := maps.Clone(p.Product)

	for name, value := range set {
		if name == fieldTime {
			p.Sensors[name] = value

			continue
		}

		p.Product[name] = value
	}

	if mode, changed := set["fmod"]; changed {
		p.Product["fnst"] = off
		if mode == fan {
			p.Product["fnst"] = fan
		}
	}

	if p.hotCool {
		p.Product["fnst"], p.Product["hsta"] = off, off

		if p.Product["fpwr"] == powered {
			p.Product["fnst"] = fan
		}

		if p.Product["hmod"] == heat {
			p.Product["hsta"] = heat
		}
	}

	pairs := map[string][2]string{}
	for name, now := range p.Product {
		pairs[name] = [2]string{before[name], now}
	}

	return pairs
}

// handle answers one message published to the purifier's command topic.
func (p *Purifier) handle(message mqtt.Message) {
	if message.Topic != p.command {
		return
	}

	asked := Received{QoS: message.QoS}
	if json.Unmarshal(message.Payload, &asked) != nil {
		return
	}

	p.mu.Lock()
	p.received = append(p.received, asked)

	var (
		kind, name string
		fields     any
	)

	switch asked.Message {
	case "REQUEST-CURRENT-STATE":
		kind, name, fields = "CURRENT-STATE", "product-state", maps.Clone(p.Product)
	case "REQUEST-PRODUCT-ENVIRONMENT-CURRENT-SENSOR-DATA":
		kind, name, fields = "ENVIRONMENTAL-CURRENT-SENSOR-DATA", "data", maps.Clone(p.Sensors)
	case "STATE-SET":
		kind, name, fields = "STATE-CHANGE", "product-state", p.change(asked.Data)
	}

	p.mu.Unlock()

	if kind != "" {
		p.Say(kind, name, fields)
	}
}

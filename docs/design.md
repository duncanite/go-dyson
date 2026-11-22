# go-dyson: the design

Research into Dyson connected air purifiers, done on 2026-11-15 from public sources, and
the module built from it. The module follows the reference implementation's code; nothing
here was run against a purifier.

In short: each purifier runs its own MQTT broker on the LAN, and with the purifier's
credential a client reads its sensors and drives it there, with no cloud at run time. The
credential is on the purifier's Wi-Fi sticker where it has one, and otherwise comes from
the owner's Dyson account, once. Dyson documents none of this. It is the route every open
implementation takes, one of them in Go.

## The device

A purifier is a Wi-Fi client that runs an MQTT broker and announces it over mDNS as
`_dyson_mqtt._tcp`, its instance named by product type and serial, `475_<serial>`
([libdyson-neon's discovery](https://github.com/libdyson-wg/libdyson-neon/blob/28063efd190c690e624f96bc214b6fc2dd4c6489/libdyson/discovery.py)
and [its test](https://github.com/libdyson-wg/libdyson-neon/blob/28063efd190c690e624f96bc214b6fc2dd4c6489/tests/test_discovery.py);
[opendyson's scan](https://github.com/libdyson-wg/opendyson/blob/04524be34f052531799c63bf2139847b63582359/internal/lan/scan.go)
reads the same name). The same topics are reachable
through Dyson's cloud, which is how the phone app reaches a purifier from outside.

## Ways in

### The purifier's MQTT broker (local)

From the source of [libdyson-neon](https://github.com/libdyson-wg/libdyson-neon)
(`dyson_device.py`, `utils.py`, `cloud/utils.py`), the reference implementation:

- Port 1883, plain TCP, MQTT 3.1. The user name is the serial, the password the
  purifier's credential.
- State arrives on `<type>/<serial>/status/current`; commands go to
  `<type>/<serial>/command`. Messages are JSON: a request for the current state, a request
  for the sensor data, and `STATE-SET` with the keys to change.
- `<type>` is a product code that has to be exact, suffix included: 438 and its variants
  for the TP04, TP07, TP09 and TP11, 527 and its variants for the HP04, HP07 and HP09, 358
  for the humidifying models, 475, 469 and 455 for the older Link generation, 664 for the
  Big+Quiet.
- Read: particulates, VOC and NO2 indices, temperature, humidity, formaldehyde on the
  models that sense it, filter life, the fan's state. Do: power, speed, auto mode,
  oscillation and its angles, night mode, the sleep timer, heat on the Hot+Cool models.
- The credential, two ways:
  - **From the sticker**: base64 of the SHA-512 of the Wi-Fi password printed on the
    purifier's label or in its manual, the serial and product type read from the same
    label's network name. No cloud. Models since about 2020 often carry no such label.
  - **From the account**: the device list of Dyson's app API carries the credential
    encrypted under a fixed key; decrypted, it holds the same password. Fetched once.
- The Link generation uses another set of keys for its state.

[ha-dyson](https://github.com/libdyson-wg/ha-dyson), the Home Assistant integration built
on it, notes a firmware fault on the TP07, TP09, HP07 and HP09: connections that are not
closed fill the broker until the purifier is power-cycled. A client keeps one connection
and closes it.

### Dyson's account API and cloud MQTT (unofficial)

Dyson publishes no API. The app's own, as the open projects use it: a login by email
with a one-time code sent by mail, a device list, and credentials for an MQTT endpoint in
the cloud that carries the same messages as the local broker
([the API as an OpenAPI description](https://github.com/libdyson-wg/appapi)). The login
cannot run unattended, and Dyson can change it.

### Matter

Dyson added Matter by a firmware update in October 2025 to its 2025 De-NOx models only
([Matter Alpha's report](https://www.matteralpha.com/news/dyson-embraces-matter-for-its-high-end-air-purifiers)):
an air purifier with its fan and filters, air quality sensors, temperature and humidity.
The earlier generations have none, and no HomeKit.

## Two ways in, compared

| | The purifier's broker | The cloud |
|---|---|---|
| Supported by Dyson | no | no |
| Local at run time | yes | no |
| Needs | the credential: the sticker, or one fetch from the account | the account, a one-time code at each login |
| Gives | every sensor and control | the same |
| Risk | firmware changes; the broker's connection fault | Dyson changing the account API |

## What this module is

**A client of the purifier's own broker**, for the Pure Cool Link, the TP02, product type
475: the purifiers it was written for, which carry their Wi-Fi stickers. Local, with no
account.

- `dyson`: `FromSticker` makes a purifier's identity from the network name and the
  password on its sticker. `Recognize` and `Find` say what a purifier is on the network.
  `Dial` connects; `State` and `Environment` ask for how it is
  set and what its sensors read; `Next` reads what it says as it says it; `SetMode`,
  `SetSpeed`, `SetOscillation`, `SetNightMode` and `SetSleepTimer` drive it; `Done` and
  `Err` say when the connection ended, and why.
- `mqtt`: an MQTT client of this module's own design, and `mqtt/mqtttest`, a broker for
  tests.
- `dyson/dysontest`: a purifier for tests, on that broker.
- `cmd/dyson`: the command, over `internal/cli`.

### Decisions

- **The credential comes from the sticker**, and the sticker from the environment:
  `DYSON_SSID` and `DYSON_WIFI_PASSWORD`. Nothing is asked of Dyson's cloud. The account
  route, for a purifier with no sticker, is not built.
- **Only the type 475 is spoken.** The reference implementation's file for the Link
  generation was read, `dyson_pure_cool_link.py`: its state is `fmod`, `fnst`, `fnsp`,
  `oson`, `nmod`, `rhtm`, `filf`. Later generations name their state otherwise; another
  type is refused, with nothing sent.
- **MQTT 3.1, as both implementations pin it**: libdyson-neon asks its library for
  `MQTTv31`, and [opendyson](https://github.com/libdyson-wg/opendyson) sets Paho's
  protocol version to 3. Whether a purifier takes MQTT 3.1.1 is not known.
- **One connection a command, closed at its end.** The later models' brokers run out of
  connections; nothing says the TP02's does not.
- **A `Purifier` is for one goroutine.** What the purifier says arrives on one
  subscription; `State`, `Environment` and `Next` all read it, and each passes over what
  it did not ask for.
- **A `Purifier` says that its connection ended; it never connects again by itself.**
  `Done` and `Err` are the connection's own, for a caller that only sends a command from
  time to time and would otherwise hold a dead connection until its next one failed. A
  purifier that went is the caller's to wait for and to `Dial` again.
- **A change returns when the broker took it**, at quality of service 1, as the
  reference implementation publishes it. What the purifier then does, it says, and
  `Next` reads.
- **The library says what a purifier is on the network, and opens no socket to look.**
  `Service` is what a purifier announces, and `Recognize` reads an instance's name into
  the serial and the product type its sticker gives, on a value, with no mDNS library
  behind it. `Find` searches through a `Browser` its caller lends, an interface of this
  package in standard types: mDNS's port is one to a host, and belongs to the program. An
  announcement proves nothing and carries no secret: the credential stays the sticker's.
  No TXT entry is read, since neither implementation reads one.
- **Not built**: the search itself, in the command, which still takes an address; the
  other product types, the account route, the air quality target, the monitoring switch,
  resetting the filter, heat.

### MQTT, and the library under it

- **This module speaks MQTT through an API of its own**, package `mqtt`, shaped for Go
  and not after the library that carries the bytes. A `Conn` is one session over one
  connection and is never dialled again behind its caller; every call takes a context
  and returns when the broker has answered; messages are read from a `Subscription`, in
  order, and a subscription nobody reads holds the connection back and loses nothing.
  One whose grant did not come before its caller gave up is ended there, so that it
  cannot. The package may become a module of its own.
- **The bytes are carried by [Eclipse Paho](https://github.com/eclipse-paho/paho.mqtt.golang)**,
  the one maintained Go client found that speaks MQTT 3.1. Package `mqtt` and its test
  broker alone name it, a rule of `.lint-go.yaml` holds every other package to that, and
  none of its types cross the API.
- **Paho's license.** Its `LICENSE` offers it under the EPL-2.0 or the EDL-1.0, at the
  taker's choice. The EDL-1.0 is the BSD-3-Clause's text, which this project accepts,
  and it is the one Paho is taken under. The license lane reads the file as EPL-2.0
  alone and is told to pass the module over, in `.lint-go.yaml`, with this reason.
- The other Go clients looked at speak MQTT 3.1.1 or 5 only:
  [at-wat/mqtt-go](https://github.com/at-wat/mqtt-go),
  [natiu-mqtt](https://github.com/soypat/natiu-mqtt), and Eclipse's own
  [paho.golang](https://github.com/eclipse-paho/paho.golang).

## Open questions

- Whether the purifiers and the machine that runs the command are on one network. The
  command takes an address, so mDNS need not pass.

## Not verified

- Nothing above was observed on a purifier, and the module was run against its own
  stand-in only.
- Whether a TP02 takes a second connection while the Dyson app holds one.
- What a purifier answers a change it refuses: the reference implementation waits for
  no answer.
- The unit of the heating target, read as tenths of a kelvin by inference.
- The particulates' and volatile compounds' units: the reference implementation calls
  them "unknown unit".
- Reports that some recent models refuse local connections, the Big+Quiet with
  formaldehyde sensing among them, give no cause.
- What the MyDyson app itself sends on the LAN.

## Sources

- [libdyson-neon](https://github.com/libdyson-wg/libdyson-neon): the protocol, the
  product codes, both ways to the credential.
- [ha-dyson](https://github.com/libdyson-wg/ha-dyson): setup, the sticker's absence on
  newer models, the connection fault.
- [opendyson](https://github.com/libdyson-wg/opendyson): a Go implementation; mDNS; the
  cloud's MQTT.
- [appapi](https://github.com/libdyson-wg/appapi): the account API described.
- [matterbridge-dyson-robot](https://github.com/thoukydides/matterbridge-dyson-robot):
  models and their product codes, models without local access.
- [Matter Alpha, Dyson embraces Matter](https://www.matteralpha.com/news/dyson-embraces-matter-for-its-high-end-air-purifiers):
  which models have Matter and what it carries.

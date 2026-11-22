# go-dyson: the design

Research into Dyson connected air purifiers, done on 2026-11-15 from public sources, and
the module built from it. The module follows the reference implementation's code. Its
search, its reads and some of its commands have been run against purifiers, as [Observed
on purifiers](#observed-on-purifiers) says, and [Not verified](#not-verified) what has
not.

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
475, and for the Pure Hot+Cool of the generation after it, product type 527: the
purifiers it was written for. Local, with no account for a purifier that carries its
Wi-Fi sticker.

- `dyson`: `FromSticker` makes a purifier's identity from the network name and the
  password on its sticker. `Recognize` and `Find` say what a purifier is on the network.
  `Dial` connects; `State` and `Environment` ask for how it is
  set and what its sensors read; `Next` reads what it says as it says it, each thing
  with the message whole, and `Ask` has it say both without waiting; `SetMode`,
  `SetSpeed`, `SetOscillation`, `SetNightMode` and `SetSleepTimer` drive it, `SetHeat`
  and `SetHeatTarget` a Hot+Cool's heater; `Done` and `Err` say when the connection
  ended, and why.
- `dyson/account`: the identity of each purifier of a Dyson account, fetched with a
  login by a one-time code: what a sticker would have given, for a purifier that has
  none. `dyson/account/accounttest` is the account API for tests.
- `mqtt`: an MQTT client of this module's own design, and `mqtt/mqtttest`, a broker for
  tests.
- `dyson/dysontest`: a purifier for tests, on that broker.
- `cmd/dyson`: the command, over `internal/cli`, which holds the host's mDNS, through
  [go-mdns](https://github.com/forkcloser/go-mdns), and lends it to the library.

### Decisions

- **The credential comes from the sticker**, and the sticker from the environment:
  `DYSON_SSID` and `DYSON_WIFI_PASSWORD`. Nothing is asked of Dyson's cloud for a purifier
  that has one.
- **A purifier with no sticker is asked of the account, once.** Models since about 2020
  carry no Wi-Fi sticker, and the open implementations know no other source for their
  credential than the owner's account: neither uses a code printed on a label, nor
  Bluetooth. `dyson/account` does the app's own login, as
  [libdyson-neon's account client](https://github.com/libdyson-wg/libdyson-neon/blob/28063efd190c690e624f96bc214b6fc2dd4c6489/libdyson/cloud/account.py)
  does it: a first request the API wants before any other, the account's status, a code
  Dyson sends by mail against a challenge, then the code, the challenge and the
  account's password for a bearer token, and the device list under it. Each device's
  local credential is base64 of AES-256 in CBC mode under a key and a vector that are
  the same for every device and published
  ([its decryption](https://github.com/libdyson-wg/libdyson-neon/blob/28063efd190c690e624f96bc214b6fc2dd4c6489/libdyson/cloud/utils.py)),
  of JSON that holds the broker's password. The package keeps nothing and writes
  nothing; its challenge, its session and its errors show no address, password, code,
  token or credential. A device of the list that does not read, a credential that does
  not decrypt among them, is left out and said, and the others are given: the reference
  does the same, and one device Dyson writes otherwise is not to hide a purifier. With
  no client given it follows no redirect, which would carry the login's body, the
  password in it, to wherever it pointed. A device's type is the list's, with its variant's letter for the
  generations that have variants; the reference also reads a variant out of the
  firmware's version when the list gives none, and this does not.
- **The command takes an identity that is no sticker's from the environment too**:
  `DYSON_SERIAL`, `DYSON_TYPE` and `DYSON_CREDENTIAL`, all three or none. Its `account`
  command reads the account from `DYSON_ACCOUNT_EMAIL`, `DYSON_ACCOUNT_PASSWORD` and
  `DYSON_ACCOUNT_COUNTRY`, asks for the code where it is typed, and prints each
  purifier's three values: they have to cross to the environment by a person's hand,
  since nothing here writes a file, and what is printed opens the purifiers. Every
  run has Dyson send a code, which it refuses to do too often. `--timeout` bounds what
  is asked of Dyson, before the code and after it, and what is typed where the code goes
  is read no further than a code may be.
- **Only the type 475 is spoken.** The reference implementation's file for the Link
  generation was read, `dyson_pure_cool_link.py`: its state is `fmod`, `fnst`, `fnsp`,
  `oson`, `nmod`, `rhtm`, `filf`. Later generations name their state otherwise; another
  type is refused, with nothing sent.
- **Two generations are spoken, by their product type.** The Link's, the type 475, as
  the reference implementation's `dyson_pure_cool_link.py` reads it: its state is `fmod`,
  `fnst`, `fnsp`, `oson`, `nmod`, `rhtm`, `filf`. And the generation after it with a
  heater, the type 527 and its variants 527E, 527K and 527M, which the reference speaks
  to as one, as its
  [`dyson_pure_cool.py`](https://github.com/libdyson-wg/libdyson-neon/blob/28063efd190c690e624f96bc214b6fc2dd4c6489/libdyson/dyson_pure_cool.py)
  and the heating device of its
  [`dyson_device.py`](https://github.com/libdyson-wg/libdyson-neon/blob/28063efd190c690e624f96bc214b6fc2dd4c6489/libdyson/dyson_device.py)
  read it: a power `fpwr` and an auto mode `auto` where the Link has one mode; two
  filters in percent, `hflr` and `cflr`; the heater's `hmod`, `hsta` and `hmax`; the
  particulates as `p25r` or `pm25` and `p10r` or `pm10`, and two indices in tenths,
  `va10` and `noxl`. Another type is refused, with nothing sent: the type 438, the Pure
  Cool of that same generation, is the same reading with no heater, and is not spoken
  because none was there to run it against.
- **One `State` for both.** `Mode` is the Link's own word, and the later generation's
  power and auto mode read into it: off, on, or on and left to the purifier. What only
  one generation says is a field the other leaves at nothing: the Link's hours of
  filter, the later one's two percentages and its heater. `Environment` likewise: each
  generation's own readings of the air, under their own names, since the Link's indices
  and the later one's micrograms are not the same measure.
- **`SetMode(Auto)` on the later generation sends the power with the auto mode.** The
  reference sends the auto mode alone; what a purifier that is off then does was not
  read, and auto is the fan run by the purifier, which is a fan that is on.
- **The later generation is told to turn in its own words.** By its firmware it writes
  its oscillation as `ON` and `OFF` or as `OION` and `OIOF`, and the reference tells it
  to turn in whichever it wrote, with its fan on and the angles it last said, `osal` and
  `osau`. So a `Purifier` keeps those from the last state it read, and `SetOscillation`
  asks for a state first when it has read none. The reference sends the sweep, `ancp`,
  as `CUST` every time; a purifier was seen to say a sweep of its own there, `0180`, and
  this module says back whatever it last said, so that turning changes nothing of how
  it turns.
- **The heater's target is 274 to 310 kelvin, as the reference takes it**: 0.85 to
  36.85 °C. It is sent in tenths of a kelvin and bounded in them, so a purifier set in
  Fahrenheit, as one seen was, is given any whole degree of its own scale inside that.
  What a purifier itself takes at either end was not run.
- **The command prints a target as the degree it was set to.** A purifier holds a target
  to the tenth of a kelvin, which no whole degree of either scale is: 21 °C is held as
  21.05, and 55 °F as 54.95. A target that is the nearest a purifier can hold to a whole
  degree, within half a tenth of a kelvin, is printed as that degree, in whichever scale
  it is one; any other, and every temperature a sensor reads, is printed to the tenth as
  it is. The library's `Heat.Target` is the purifier's own value, unchanged.
- **What a purifier says can be read whole.** This module reads the fields it knows, and
  a purifier says more, and may say a known thing under a name this module reads
  second: it took a program written aside to see that one sends its particulates under
  two names that disagree. An `Update` carries the message it was read from, whole, and
  `status --whole` and `watch --whole` print it: indented for the two answers, one line
  a message under watch. It is the purifier's text, printed so that it stays JSON and
  holds nothing a terminal obeys: a character a terminal would not simply draw is
  written as JSON writes one. The message this module cannot read is the one most worth
  seeing: it comes whole with its error, and is printed before the error is said; one
  that is no JSON is printed as the purifier's text.
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
- **The command shows a purifier's text and never obeys it.** An error code, a warning
  and what the broker says in refusing are whatever the purifier, or whoever answers for
  it, wrote. Every line the command prints goes through `internal/terminal`, which writes
  out, as Go writes them in a string, each character a terminal would not simply draw: a
  control or a direction mark, a line's end, a tab, a byte that is no text. A test reads
  the command's source and fails on a line printed any other way, since no test of what
  is printed could name every line there will be.
- **With no address, the command looks for the sticker's purifier.** The sticker names a
  serial, and a purifier announces its own: the command searches for `Service`, takes the
  one whose serial is the sticker's, and tries each place it says it is, in order, until
  one answers; a credential refused at one ends it, since it would be refused at all, and
  a purifier's broker counts what it is asked. The search ends as soon as that purifier
  is heard with an address, and at `--wait` otherwise. `discover` lists every purifier
  that announces itself, and asks for no sticker: what is on the network is anyone's to
  hear. An address given, by `--host`, is used as it is and nothing is searched for.
  The search trusts what it hears: the credential made from the sticker goes, in MQTT
  3.1's CONNECT, which is in the clear, to the first host on the network that announces
  the sticker's serial with an address, and a purifier tells its serial to everyone. A
  host on the network that announces that serial first is sent the credential. With
  `--host` the same credential goes in the clear to the address given, so the search
  adds who may be sent it and not how it is sent; a network nobody else is on is what
  both rest on.
- **Not built**: the other product types, the air quality target, the monitoring switch,
  resetting the filter; for the later generation, its oscillation's angles, its airflow's
  direction and its focus mode; the account API of mainland China, which is another host
  and logs in by a telephone number.

### MQTT, and the library under it

- **This module speaks MQTT through an API of its own**, package `mqtt`, shaped for Go
  and not after the library that carries the bytes. A `Conn` is one session over one
  connection and is never dialled again behind its caller; every call takes a context
  and returns when the broker has answered; messages are read from a `Subscription`, in
  order, and a subscription nobody reads holds the connection back and loses nothing.
  One whose grant did not come before its caller gave up is ended there, so that it
  cannot. Until the grant nobody can read, and a broker may send on the filter ahead of
  it: as many messages as the buffer holds are kept, and more fails the `Subscribe` with
  `ErrOverrun`, since a queue that held the connection back then would hold back the
  grant it waits for. The package may become a module of its own.
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

## Observed on purifiers

On 2026-11-22 the command's `discover` was run on a network with two purifiers, and
heard both within its wait:

- Each announces `_dyson_mqtt._tcp`, under an instance named by its product type and its
  serial, joined by an underscore, as the reference implementations read it: one of type
  475, one of type 527. The serials have the shape `Recognize` takes.
- Each gives port 1883 and one IPv4 address.

The same day the command's `status` was run with the sticker of the one of type 475,
and no address:

- The search took the purifier whose serial is the sticker's, at the address it gave.
- Its broker took an MQTT 3.1 session under the serial and the credential made from the
  sticker's password, and the subscription to what it says.
- It answered both requests, for its state and for its sensors, and every field read
  into this module's types: the fan on at speed 2 and turning, oscillation and night mode
  off, monitoring on, the filter at 0 hours with the warning `FLTR`, a temperature of
  19.5 °C, a humidity of 62 %, and 0 for particulates and for volatile compounds. The
  temperature and the humidity are what the room was, which is what reading the
  temperature as tenths of a kelvin gives.

Then `speed 10` and `off` were run on it, found the same way: the fan went to its
fastest and stopped, by the account of the person beside it. So a `STATE-SET` the
broker takes is one the purifier obeys, for the fan's mode and its speed.

Nothing had yet been said to the one of type 527.

The same day the command's `account` was run once against the account those two
purifiers are registered to: Dyson's API took the login as `dyson/account` makes it, the
code it sent by mail was typed, and the list came back with two devices, each with a
serial and a credential that decrypted. `status` was then run with the three values the
list gave for the purifier of type 475, in place of its sticker: its broker took them,
and its state and sensors were read as before. So the credential the account gives is
the one the sticker gives.

Then `status` was run on the purifier of type 527, with the three values the account's
list gave for it and no address: the search found it, its broker took the credential,
and its state and its sensors were read, every field into this module's types:

- The fan off, with a speed of 2 kept, and still; oscillation off, night mode on,
  monitoring on.
- Both filters at 23 %.
- The heater off, with a target of 12.8 °C, which is 55 °F.
- An error code, `11E1`, which the purifier gave with nothing else amiss to see. What it
  means is not known.
- A temperature of 18.1 °C and a humidity of 65 %, the room's.
- Both particulate readings at 999, with the fan off.
- The volatile compounds at 0.5 and the nitrogen dioxide at 0.0.

Then `speed 2` was run on it and `status` a minute later: the fan was on at speed 2 and
turning, so the later generation takes its speed as this module sends it. Both
particulate readings were still 999, with the fan on.

Its two answers were then read whole, as it sent them:

- The particulates come under both names at once, and do not agree: `p25r` and `p10r`
  were `0999`, `pm25` and `pm10` `0149`. This module reads `p25r` and `p10r` first, as
  the reference does, so the 999 is the purifier's own word and not a field misread. 999
  is the top of the range
  [hass-dyson](https://github.com/cmgrayb/hass-dyson/blob/main/docs/SENSORS.md) accepts
  for these readings, and
  [ioBroker's adapter](https://github.com/Grizzelbee/ioBroker.dysonairpurifier) took
  to the `r` names because they are what Dyson's app shows; whether 999 is a sensor at
  the end of its scale, or one that reads nothing, is not known, and neither is what
  the error `11E1` says.
- This purifier writes its oscillation as `ON` and `OFF`, with a sweep `ancp` of `0180`
  where the reference's recorded state has `CUST`, and angles of `0135` and `0225`.
- The state also names each filter's kind, `cflt` and `hflt`, and holds the sleep timer,
  `sltm`, which the sensors' answer holds too. Neither kind is read.

Then `oscillate on` and `oscillate off` were run on it: it turned, and it stopped, by
the account of the person beside it. It was told to turn in its own words, `ON`, with
the sweep and the angles it had said.

Last, `heat on` was run on it, and `status` after: the heater was set to heat and was
not heating, its target of 12.8 °C being under the room's 18.2 °C, and the fan was
still off. So the purifier takes the heater's mode as this module sends it, and says
apart what it is set to and what it does.

Then the rest of its commands were run on it, in three turns, each read back by
`status`:

- `heat off`, `on`, `heat 21`: half a minute later the fan was on at speed 2 and
  turning, and the heater set to heat and heating, to 21.1 °C. A target of 21 °C is
  294.15 kelvin, halfway between two tenths: it is sent as 2942, which is 21.05 °C. Every
  whole degree Celsius falls so, and was then printed a tenth above what was asked; the
  command has since printed such a target as the degree it was set to.
- `auto`, `night off`, `timer 30m`: the fan was in auto and turning, night mode off, and
  the sleep timer at 29 minutes.
- `timer off`, `night on`, `heat 12.78`, `heat off`, `on`, `speed 2`, `off`: the purifier
  was as it had been found, the fan off with a speed of 2 kept, night mode on, no timer,
  and the heater off with its target at 12.8 °C, 55.0 °F, again.

So every command this module has for the later generation was taken by a real purifier
as it is sent, the auto mode sent with the power among them.

Last, the Link's other commands were run on the purifier of type 475, in two turns, each
read back by `status`:

- `auto`, `oscillate on`, `night on`, `timer 30m`: the fan was in auto and turning, the
  purifier oscillating, night mode on, and the sleep timer at 29 minutes.
- `timer off`, `night off`, `oscillate off`, `speed 2`: the purifier was as it had been
  found, the fan on at speed 2, not oscillating, night mode off, no timer.

In both its volatile compounds read nothing, where they had read 0 before: which of
`OFF`, `INIT` and `FAIL` the sensor then said is not known.

## Open questions

- Whether the purifiers and the machine that runs the command are on one network. The
  search is mDNS, which does not leave a network; given an address, the command needs
  none.

## Not verified

- `on` on a purifier of type 475, and on either type `watch` and what a purifier says
  unasked: run against the module's own stand-in only.
- What the account's API answers when it refuses, a code asked for too often among it:
  read off the reference implementation, and run against a stand-in of the API only.
- What a purifier of type 527 takes at either end of the heat target's range.
- What a particulate reading of 999 is, and what the error `11E1` is: see [Observed on
  purifiers](#observed-on-purifiers).
- Whether a TP02 takes a second connection while the Dyson app holds one.
- What a purifier answers a change it refuses: the reference implementation waits for
  no answer.
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

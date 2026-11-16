# go-dyson: the design

Research into Dyson connected air purifiers, done on 2026-11-15 from public sources, to
decide what a Go module for them would be. Nothing is built and nothing here was run
against a purifier. The decisions at the end are open.

In short: each purifier runs its own MQTT broker on the LAN, and with the purifier's
credential a client reads its sensors and drives it there, with no cloud at run time. The
credential is on the purifier's Wi-Fi sticker where it has one, and otherwise comes from
the owner's Dyson account, once. Dyson documents none of this. It is the route every open
implementation takes, one of them in Go.

## The device

A purifier is a Wi-Fi client that runs an MQTT broker and announces it over mDNS as
`_dyson_mqtt._tcp`, its instance named by product type and serial
([opendyson](https://github.com/libdyson-wg/opendyson)). The same topics are reachable
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

## What this module would be

Not decided. The options, most local first:

1. **A client of the purifier's broker**: discovery over mDNS or a given address, the
   state and sensors decoded by product code, the commands. The credential is
   configuration, derived from the sticker by the command or fetched once from the account
   by a login subcommand.
2. **The cloud's MQTT**, the same messages over another transport, where a purifier
   refuses local connections.

[opendyson](https://github.com/libdyson-wg/opendyson) is Go, and does the login, the
device list and listening on either transport; it sends no command.

## Open questions

- The model code of each purifier, on the label under its base (TP07, HP04 and the like):
  it sets the product code and the keys.
- Whether each has a Wi-Fi label with a network name and a password.
- Whether the purifiers and the machine that runs the command are on one network, with
  mDNS passing.

## Not verified

- Nothing above was observed on a purifier.
- The unit of the heating target, read as tenths of a kelvin by inference.
- The Link generation's keys: the file that holds them was not read.
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

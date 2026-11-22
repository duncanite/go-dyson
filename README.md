# go-dyson

A Go library and command for the Dyson Pure Cool Link purifier, the TP02, on the LAN: each
purifier runs its own MQTT broker, and with the credential on its Wi-Fi sticker a client
reads its state and its sensors there, and drives its fan. No cloud, and no account for a
purifier that carries its sticker.
Written against the reference implementation the open projects share. Its search, its
reads and two of its commands have been run against a purifier; the rest has not:
[docs/design.md](docs/design.md) says what was read and what was seen.

The command reads the purifier's sticker from the environment and takes it as no flag:
`DYSON_SSID`, the network name printed on it (`DYSON-<serial>-475`), and
`DYSON_WIFI_PASSWORD`, the password beside it. The purifier's address goes in `--host` or
`DYSON_HOST`; with neither, the purifier the sticker names is searched for on this
network. With the sticker set:

```bash
just dyson discover        # the purifiers that announce themselves here; needs no sticker
just dyson status          # how it is set, and what its sensors read
just dyson on              # the fan on; also off
just dyson auto            # let the purifier run the fan as the air needs
just dyson speed 4         # the fan on at a speed, 1 to 10
just dyson oscillate on    # turn from side to side; off stops it
just dyson night on        # the quiet, dimmed mode
just dyson timer 2h        # turn itself off after a time; off ends the timer
just dyson watch           # print what it says as it says it, until interrupted
```

Each purifier has its own sticker and its own address: two purifiers are two
environments. Only the Pure Cool Link's product type, 475, is spoken.

A purifier with no Wi-Fi sticker, as those made since about 2020, is named by three
values in place of the sticker's two: `DYSON_SERIAL`, `DYSON_TYPE` and
`DYSON_CREDENTIAL`. The owner's Dyson account gives them, once:

```bash
just dyson account         # each purifier of the account, with the three values that open it
```

It reads the account from `DYSON_ACCOUNT_EMAIL`, `DYSON_ACCOUNT_PASSWORD` and
`DYSON_ACCOUNT_COUNTRY` (two letters: `US`, `GB`, `FR`), has Dyson send a one-time code
by mail, and asks for it. What it prints opens the purifiers: it is to be kept as a
password is. Dyson refuses to send codes too often, so it is not a command to run again
and again.

The module speaks MQTT through a package of its own, [`mqtt`](mqtt/mqtt.go), over Eclipse
Paho, which nothing else here names.

[docs/design.md](docs/design.md) is the research the module starts from, what the
purifiers offer and the ways into them, and the shape of the module with its decisions.

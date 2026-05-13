# rmt8l

A CLI for talking to a [Radiomaster T8L](https://www.radiomasterrc.com/products/t8-lite-elrs-rc-transmitter) radio transmitter over USB.

The T8L is a screenless ELRS radio whose configuration is reachable only via a custom Radiomaster protocol while the radio is in management mode (**hold M + power for 3 seconds** — same combo as firmware update). In normal runtime mode the radio doesn't expose USB at all.

`rmt8l` is the sibling of [`bfctl`](https://github.com/justincampbell/bfctl): same shape, same conventions, same release/install story. stdout is data, stderr is for humans, exit codes are stable.

## Installation

### Go

```sh
go install github.com/justincampbell/rmt8l@latest
```

### From source

```sh
git clone https://github.com/justincampbell/rmt8l && cd rmt8l && make install
```

## Usage

Put the radio in management mode (hold **M** while pressing power for 3 s), plug in USB, then:

```sh
rmt8l backup                              # pull TX (+RX if bound) + device-state → {tx,rx}-settings.{bin,txt} + device-state.{bin,txt}
rmt8l backup --out-dir radios/t8l         # …into a specific directory
rmt8l backup --from-file tx.bin           # …decode an existing TX dump without touching the radio
rmt8l backup --rx-from-file rx.bin        # …same, for an RX dump
rmt8l backup --device-from-file dev.bin   # …same, for a device-state dump
rmt8l info                                # print volume / firmware versions / slider midpoint / etc.
rmt8l info --json                         # …as JSON
rmt8l info --from-file dev.bin            # …from a saved device-state .bin
rmt8l ports                               # list detected Radiomaster radios
rmt8l ports --json                        # …as JSON
rmt8l version
```

The `.bin` is the byte-stable raw response from the radio. The `.txt` is a decoded, single-record-per-block view of every setting — written so `git diff` localises any change to a few lines.

The RX dump (`A5 22`) only exists when a receiver is bound to the radio and powered up. If no RX is visible, `rmt8l backup` writes the TX pair and skips the RX pair with a stderr note; this is normal, not an error.

The device-state dump (`A5 55 1D`, parsed via `parseDeviceInfoPacket` in the configurator) covers the radio's own state — current volume, slider midpoint, calibration flag, firmware versions, chip-ID serial. It's the half a factory reset actually touches.

If more than one Radiomaster radio is plugged in, pass `--port`:

```sh
rmt8l backup --port /dev/cu.usbmodemRADIOMASTER1
```

## What it pulls

`rmt8l backup` covers both sides of the ELRS link plus the radio's device-state:

- **TX-side module settings** (`A5 11 00 0D 0A`) — everything in the radio's "ExpressLRS" menu: Packet Rate, Telem Ratio, Switch Mode, Link Mode, Model Match, TX Power, VTX Administrator, Bind, Bad/Good counter, and the bind UID.
- **RX-side settings** (`A5 22 00 0D 0A`) — the bound receiver's parameters, polled over the air. Only present when an RX is powered and bound; the empty case is a clean skip.
- **Device-state** (`A5 55 1D 0D 0A`) — the radio's own state: volume, slider midpoint, calibration flag, firmware versions, key-mode assignments, chip-ID serial. Surfaced standalone via `rmt8l info`.

A few things still live elsewhere and aren't pulled yet:

- **Channel map** (which switch → which AUX) — surfaced via the `0x44` channel-map packet (`tryParseChannelMapPacket`).
- **Live channel snapshot** (current stick/switch values) — `A5 55 02`.

These are tracked for follow-up work. The `.bin` we save today is byte-stable across reboots and survives a factory-reset round-trip cleanly, so it's a sound base.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Generic error |
| 2 | Usage error |
| 3 | No Radiomaster radio found (likely not in M+power mode) |
| 4 | More than one radio found (use `--port`) |
| 5 | Port already in use (Chrome / web configurator likely holding it) |

## How it works

The radio enumerates over USB as a CDC ACM device at VID:PID `19f5:5740`, baud 420000. It emits unsolicited link-strength (`0x67 0x0C …`) and progress (`0x23 0x03 …`) packets continuously while idle; `rmt8l` skips past them to find the real response, which is sentinel-prefixed (`0x56`) and CRLF-terminated.

The TX settings dump is a sequence of chunked records — one chunk per `EA <len> <marker> EA EE <payload> <CRC8>` frame, with parameter records spanning multiple chunks (joined by a shared `pid` byte). The RX dump uses the same framing. The decoder is a faithful port of [`fpv/scripts/t8l-backup.py`](https://github.com/justincampbell/fpv/blob/main/scripts/t8l-backup.py), tested byte-for-byte against a captured fixture.

The device-state response uses a different framing (`0xFF <len> <payload> <CRC8>`, no sentinel, no CRLF). Both framings are validated against captured fixtures in `internal/proto/testdata/`.

## License

[MIT](LICENSE)

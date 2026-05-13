# rmt8l

A Go CLI for talking to a [Radiomaster T8L](https://www.radiomasterrc.com/products/t8-lite-elrs-rc-transmitter) radio transmitter over USB CDC ACM. Sibling project to [`bfctl`](https://github.com/justincampbell/bfctl); same shape, same conventions, same release/install story.

## Workflow rules

**Run `make install` after every change.** The user runs the binary directly between turns; the installed copy needs to be current. After any edit to Go source, the Makefile, the goreleaser config, or anything else that affects the built binary, run `make install` and verify it succeeds before reporting the change as done. (`make install` puts `rmt8l` and `rmt8l@<version>` on `$GOBIN`, so the version lifts directly from `git describe`.)

**Run `make lint` locally before pushing.** Use the Makefile target — never `go vet` alone — so it matches what CI runs (`golangci-lint`). CI failures on lint are avoidable; catch them locally.

If the change is documentation-only (`README.md`, `CLAUDE.md`, etc.), `make install` and `make lint` are not required.

## Tech stack

- **Language**: Go (module `github.com/justincampbell/rmt8l`)
- **Serial**: `go.bug.st/serial` and its `enumerator` subpackage
- **CLI**: stdlib `flag` (one `flag.NewFlagSet` per subcommand). No cobra/viper.
- **Build/release**: goreleaser → multi-arch darwin binaries + Homebrew tap (`justincampbell/homebrew-tap`)

## Architecture

```
main.go                       — subcommand dispatch + per-command flag parsing
internal/radio/radio.go       — port discovery + Pull primitives (USB-CDC at 420000 baud)
internal/proto/proto.go       — chunked-response framing: 0x56 sentinel walker + parameter parser (A5 11 / A5 22)
internal/proto/render.go      — diff-friendly txt renderer for the chunked TX/RX response
internal/proto/devinfo.go     — 0xFF device-state packet: parser + CRC8 + RenderDeviceState
internal/proto/testdata/      — golden fixtures: tx-settings.{bin,txt} + device-state.{bin,txt} from a real T8L
```

`internal/proto/proto_test.go` decodes the TX fixture and asserts the rendered txt matches byte-for-byte; `devinfo_test.go` does the same for device-state plus CRC-rejection tests. These are load-bearing — once they pass, live pulls are mechanical.

Subcommands (current): `backup`, `info`, `ports`, `version`.

Reserved subcommand names (don't repurpose without renaming): `channels` (live stick/switch snapshot — issue #3), `set`, `restore` (write path — issue #5).

`backup` pulls all three frame types and writes them into `--out-dir` (default `.`):

- TX side: sends `A5 11 00 0D 0A` and writes `tx-settings.{bin,txt}`.
- RX side: sends `A5 22 00 0D 0A` and writes `rx-settings.{bin,txt}` if a receiver is bound and answered. With no RX bound the radio goes silent (no bytes at all); `radio.PullRXSettings` returns `(nil, nil)` and `backup` skips the RX pair with a stderr note — exit code 0.
- Device-state: sends `A5 55 1D 0D 0A`, extracts the `0xFF <len> <payload> <CRC8>` packet out of the surrounding live-noise, and writes `device-state.{bin,txt}`. The .bin contains the framed packet only (noise stripped), so it's byte-stable across runs.

`--from-file <path>`, `--rx-from-file <path>`, and `--device-from-file <path>` each decode an existing .bin for that frame type without touching the radio. Useful for tests and for re-rendering after a renderer change. Passing any one of them puts `backup` into offline mode — only the side(s) with an explicit file are processed; the others are skipped. Pass none to do a full live pull.

`info` is the standalone surface for the device-state packet. `rmt8l info` prints the same diff-friendly text the .txt contains; `--json` emits a structured object; `--from-file` re-renders a saved .bin.

`ports` lists USB serial ports matching VID:PID `19f5:5740`. `--json` for structured output.

## Talking to the radio — non-obvious bits

- **The radio only exists in management mode.** Hold `M` while pressing power for 3 seconds to enter it. In normal runtime mode there is no USB device at all — `rmt8l ports` returns empty and exits 3. Same combo enters the firmware-update flow (different baud, different protocol).
- **Identification**: USB CDC ACM, VID:PID `19f5:5740`, macOS device name `/dev/cu.usbmodemRADIOMASTER1`. The same VID/PID is likely shared with the M8L, Pocket, Boxer, and TX12 variants — `Connection.html` has explicit `if (deviceName == "RadioMaster M8L")` branches that imply this. When adding a second radio model, check whether the protocol forks or just renders differently.
- **Baud**: 420000 for settings I/O. The configurator uses 460800 for firmware update — different code path, not implemented here.
- **Chrome WebSerial holds the port exclusively** while the Radiomaster web configurator tab is open. If `rmt8l` gets "resource busy", that's the cause — close the tab and retry. `reportRadioErr` in `main.go` maps this to exit 5 with a stable hint.
- **The radio emits unsolicited telemetry continuously** while idle in management mode: 14-byte `0x67 0x0C …` link-strength packets and 5-byte `0x23 0x03 …` progress packets. These are not bugs; they're how the radio is. `proto.FindPacket` skips past them to find the `0x56` sentinel that prefixes the real response.
- **The radio goes completely silent on `A5 22` when no RX is bound and powered.** Not "still emitting noise but no settings packet" — *no bytes at all*, the link-strength stream stops too. That's why `radio.PullRXSettings` returns `(nil, nil)` on a `errNoBytes` read (rather than letting the caller hunt for a sentinel in noise that never arrives), and why it's only safe to call AFTER a TX pull has proven the radio is talking — otherwise a silent radio could be misread as "no RX" when actually the radio is unplugged or out of mode.
- **The device-state response (`0xFF`) is framed completely differently** from the chunked TX/RX response: no 0x56 sentinel, no CRLF terminator, and a single CRC8 over the payload (table lifted verbatim from `crc8tab_js` in Connection.html). The transport reads bytes until idle gap, then `proto.FindDeviceInfo` scans for `0xFF <len> <payload> <CRC8>` — CRC validation is the only way to disambiguate the real packet from the 0xFF bytes that appear in `0x67` link-noise payloads.
- **The serial-number field in the device-info packet is a chip ID, not a sticker number.** The 20-byte slot contains apparently random bytes (often mixing printable ASCII with control bytes), padded with `0x00`. The configurator and `proto.RenderDeviceState` both render it as the lower-case hex of the bytes with trailing zeros stripped — matching `serialNumStr` in Connection.html, line ~9842.
- **Response framing**: `<noise> 0x56 <type:1> <prefix:1> <chunks…> <body_CRC_lo> <body_CRC_hi> 0x0D 0x0A`. The body CRC algorithm is undocumented and not validated — the Python prototype doesn't validate it either, and rejecting valid dumps for a wrong CRC formula would be worse than skipping the check. If/when we add validation, the `crc8tab_js` table in Connection.html should be copied verbatim rather than identified from scratch.
- **Chunk framing**: `EA <len> <marker> EA EE <payload[len-4]> <CRC8>`. `<len>` counts from the byte after itself up to and including the trailing CRC8, so total chunk size is `len + 2`. Markers seen: `0x29` for the device-info chunk, `0x2B` for parameter chunks. The Python prototype's walker calls the CRC byte "crc" but indexes it at `body[i]` — i.e. as if it belonged to the *next* chunk. The Go walker fixes this naming: `Chunk.CRC8` is the trailing byte of its own chunk. (Functionally equivalent; just more obvious when reading the code.)
- **Parameter records can span multiple chunks.** All chunks for one record share the same `pid` (byte 0 of the chunk payload) and use a `seq` byte (byte 1) that counts down from N to 0. `proto.MergeChunks` concatenates `payload[2:]` of each chunk in arrival order (which is also seq-descending) to recover the full record body.
- **Parameter body** (after merging): `parent:u8 dataType:u8 name\0 <type-specific…>`. `dataType`'s low 7 bits are the base type (`parseParamData` in Connection.html: 0–3 numeric, 8 float, 9 selection, 10 string, 11 folder, 12 info, 13 command); the high bit is the `hidden` flag, which marks live-updating fields like the `Bad/Good` counter — these are ephemeral and should not appear in diffs.
- **TEXT_SELECTION option strings** are `;`-separated. Two raw bytes are special: `\xC0` for switch position "low/below mid" (rendered as `↓`) and `\xC1` for "high/above mid" (`↑`). `proto.decodeOptions` substitutes these so the txt is readable; everything else outside printable ASCII is quoted as `\xNN`.
- **The .bin is almost byte-stable** between dumps of the same radio at the same state. Known variability across two consecutive dumps: the `Bad/Good` live counter and the CRC8 of the chunk that holds it — exactly two bytes. The 1162-byte fixture in `internal/proto/testdata/` was captured at `Bad/Good = 0/256`; live pulls today will diverge there and only there.
- **Factory reset semantics still unclear.** A "factory reset" via the web UI didn't change anything visible in the `A5 11` TX dump — not even the bind UID `d84313`. With the device-state packet now decoded too (`rmt8l info`), the next re-check should compare a pre/post-reset `device-state.bin` to see what actually moves; intuition says the calibration flag and slider midpoint, plus possibly trim-reset booleans, but nothing's been confirmed yet.

## Request catalogue (host → radio)

Format: `A5 <subsys> <cmd> [args…] 0D 0A`. What's actually known:

| Subsys | Status |
|:-------|:-------|
| `0x11` | TX-side ELRS module settings refresh — implemented by `rmt8l backup`. |
| `0x22` | RX-side ELRS settings refresh — empty unless an RX is bound and powered (issue #4). |
| `0x55` | "general" subsystem: channels, volume, calibration, model-match, etc. `0x1D` (device-state pull) implemented by `rmt8l info` / `backup`. Most others still unknown (issue #5). |
| `0x12`, `0x33`, `0x66`, `0x70` | Observed in JS, semantics unknown. |

Subsystem `0x55` commands seen in Connection.html (most still need verification on hardware):

| Cmd | Semantics |
|:----|:----------|
| `0x02` | Channel snapshot (16ch × 11-bit packed, 22B payload). |
| `0x19 <v>` | Set volume (0–25; displayed × 4 in UI). |
| `0x1D` | Request device-state packet (`0xFF` response). Implemented by `rmt8l info` / `backup`. |
| `0x10 <mode>` | Set some mode. |
| `0x2D <ch>`, `0x2E <ch+1>` | Channel-N related. |
| `0x04`, `0x0D`, `0x0E`, `0x1B`, `0x1F`, `0x22`, `0x24`, `0x25`, `0x26`, `0x27`, `0x2A`, `0x2B`, `0x30`, `0x31`, `0x32` | Partial / unknown. |

Outside subsystem `0xA5`: single-byte commands `[0x33]`, `[0x34]`, and a four-byte `[0x60 0xF1 0x55 0x55]` — likely state-machine pings; investigate when needed.

## Agent-friendly conventions

- **stdout = data, stderr = humans.** Errors, hints, and progress go to stderr.
- **Stable exit codes.** `0` ok, `1` generic, `2` usage, `3` no radio (likely not in M+power mode), `4` multiple radios, `5` port in use (Chrome holding it).
- **`--json`** on `ports`. Add it to any new subcommand whose output is structured.
- **No interactive prompts.** Every decision is a flag.
- **`--port` is available on every command** that talks to the radio.
- **`--from-file` is available on any decode-only command** (currently `backup`). Lets the test suite and re-renders skip the live serial path.
- **Stable error phrasing.** Other tools (and agents) may pattern-match.

## Reference repos and files

- `~/Code/justincampbell/bfctl/` — sibling CLI; mirror its structure when in doubt.
- `~/Code/justincampbell/fpv/scripts/t8l-backup.py` — the original Python prototype. The Go decoder is a faithful port; the prototype's module docstring is the protocol spec.
- `~/Code/justincampbell/fpv/radios/t8l/` — where backups land in the `fpv` repo; the fixture in `internal/proto/testdata/` was copied from here.
- `~/Code/Radiomaster-RC/RM-Web-Page/Connection.html` — the authoritative protocol source (cloned locally from `Radiomaster-RC/RM-Web-Page` on GitHub). Key functions: `parseParamData` (param type table), `parseDeviceInfoPacket` (`0xFF` device state — issue #2), `tryParseChannelMapPacket` (`0x44` — issue #6), `readLoop` (top-level dispatcher showing all packet prefixes), `refreshSettings` / `refreshSettingsRX` (the host-side request byte sequences for TX / RX).
- `~/Code/justincampbell/fpv/CLAUDE.md` — has a section on the T8L documenting the M+power gotcha and the dump pipeline; also has channel-map war stories from the Radiomaster Pocket worth reading before tackling issue #6.

## Maintaining this file

Keep it current. Update when:
- A subcommand is added, removed, or has its semantics changed.
- A new non-obvious radio quirk is discovered.
- An exit code is added or its meaning shifts.
- A previously-unknown command on the subsystem table gets decoded.
- The release/install workflow changes.

Don't document speculative features. Reserved future subcommands get a one-line mention so naming collisions are avoided, nothing more.

## Releasing

Tags `v*` trigger `.github/workflows/release.yml`, which runs goreleaser to publish darwin-amd64 + darwin-arm64 binaries on GitHub Releases and update the Homebrew formula in `justincampbell/homebrew-tap`. Goreleaser needs `HOMEBREW_TAP_GITHUB_TOKEN` in CI secrets — already set on this repo (sourced from `~/.use/homebrew-tap/.profile`).

Local snapshot build: `make build` (versioned binary copy is created alongside).

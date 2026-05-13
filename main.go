// rmt8l is a CLI for talking to a Radiomaster T8L radio transmitter over USB.
//
// The radio only exposes USB when held in management mode (M + power for 3s).
// In normal runtime mode there's no device at all. stdout is data; stderr is
// for humans (progress, hints, errors). Exit codes are stable so scripts and
// agents can branch on them.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/justincampbell/rmt8l/internal/proto"
	"github.com/justincampbell/rmt8l/internal/radio"
)

var version = "dev"

// Stable exit codes. Agents may branch on these.
const (
	exitOK             = 0
	exitGeneric        = 1
	exitUsage          = 2
	exitNoRadio        = 3
	exitMultipleRadios = 4
	exitPortInUse      = 5
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "backup":
		os.Exit(cmdBackup(os.Args[2:]))
	case "info":
		os.Exit(cmdInfo(os.Args[2:]))
	case "ports":
		os.Exit(cmdPorts(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("rmt8l", version)
	case "help", "--help", "-h":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "rmt8l: unknown command %q\n\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `rmt8l — talk to a Radiomaster T8L radio transmitter over USB.

The radio must be in management mode (hold M while powering on for 3s) to
appear as a USB device at all.

Usage:
  rmt8l <command> [flags]

Commands:
  backup   Pull TX, RX (if bound), and device-state into {tx,rx}-settings.{bin,txt} + device-state.{bin,txt}
  info     Print the radio's device-state (volume, slider midpoint, firmware versions, …)
  ports    List detected Radiomaster radios
  version  Print version

Run 'rmt8l <command> --help' for command-specific flags.
`)
}

// ----- backup -----

// cmdBackup pulls the TX-side settings dump and (if an RX is bound) the
// RX-side dump, writing up to four files:
//
//	<out>/tx-settings.bin  — raw TX response (byte-stable across runs)
//	<out>/tx-settings.txt  — decoded TX view, diff-friendly
//	<out>/rx-settings.bin  — raw RX response (only if an RX is bound)
//	<out>/rx-settings.txt  — decoded RX view, diff-friendly
//
// The .bin is the source of truth; the .txt is what `git diff` localises
// to a few lines when a setting changes. With no RX bound, the radio
// answers `A5 22` with live-noise only (no 0x56 framing); we surface that
// as a stderr note and skip the RX files.
func cmdBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	outDir := fs.String("out-dir", ".", "directory to write {tx,rx}-settings.{bin,txt} + device-state.{bin,txt} into")
	fromFile := fs.String("from-file", "", "decode an existing TX .bin instead of pulling fresh")
	rxFromFile := fs.String("rx-from-file", "", "decode an existing RX .bin instead of pulling fresh")
	deviceFromFile := fs.String("device-from-file", "", "decode an existing device-state .bin instead of pulling fresh")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}

	// Passing any --*-from-file flag puts us in offline re-render mode: we
	// only process the side(s) with an explicit file. Live mode (no flags)
	// pulls all three. This keeps `--from-file foo.bin` from blocking on an
	// RX-side serial read when the radio isn't even plugged in.
	offline := *fromFile != "" || *rxFromFile != "" || *deviceFromFile != ""
	var portPath string
	if !offline {
		p, err := radio.Resolve(*port)
		if err != nil {
			return reportRadioErr(err)
		}
		portPath = p
	}

	// TX: decode from --from-file, or pull live, or skip (offline without --from-file).
	txPort := portPath
	if *fromFile != "" {
		txPort = ""
	}
	if code := pullAndWrite(*outDir, "tx-settings", *fromFile, txPort, radio.PullTXSettings, false); code != exitOK {
		return code
	}

	// RX: same pattern. Allow the "no RX bound" case to be a clean skip
	// when running live; if the user asked for a specific RX file, surface
	// the decode error instead of swallowing it.
	rxPort := portPath
	if *rxFromFile != "" {
		rxPort = ""
	}
	allowEmpty := *rxFromFile == ""
	if code := pullAndWrite(*outDir, "rx-settings", *rxFromFile, rxPort, radio.PullRXSettings, allowEmpty); code != exitOK {
		return code
	}

	// Device-state (0xFF). Separate path because the response is framed
	// differently (no 0x56 sentinel, single-shot CRC8 over the payload).
	devPort := portPath
	if *deviceFromFile != "" {
		devPort = ""
	}
	if code := pullAndWriteDevice(*outDir, *deviceFromFile, devPort); code != exitOK {
		return code
	}
	return exitOK
}

// pullAndWrite handles one TX or RX backup pair. If fromFile is set, that
// path's bytes are decoded; otherwise the radio is polled via pullFn. With
// allowEmpty true, a `proto.ErrNoPacket` from the radio is reported on
// stderr and the function returns exitOK without writing files — that's the
// "no RX bound" case for the RX pull.
func pullAndWrite(
	outDir, baseName, fromFile, portPath string,
	pullFn func(string) ([]byte, error),
	allowEmpty bool,
) int {
	var raw []byte
	if fromFile != "" {
		b, err := os.ReadFile(fromFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rmt8l:", err)
			return exitGeneric
		}
		raw = b
	} else if portPath != "" {
		b, err := pullFn(portPath)
		if err != nil {
			return reportRadioErr(err)
		}
		raw = b
	} else {
		return exitOK
	}

	resp, err := proto.Decode(raw)
	if err != nil {
		if allowEmpty && errors.Is(err, proto.ErrNoPacket) {
			fmt.Fprintf(os.Stderr, "rmt8l: no %s response — RX not bound or not powered, skipping\n", baseName)
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "rmt8l: decode:", err)
		return exitGeneric
	}

	binPath := filepath.Join(outDir, baseName+".bin")
	txtPath := filepath.Join(outDir, baseName+".txt")
	if err := os.WriteFile(binPath, raw, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}
	if err := os.WriteFile(txtPath, []byte(proto.Render(resp)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}
	fmt.Fprintf(os.Stderr, "rmt8l: wrote %s (%d bytes)\n", binPath, len(raw))
	fmt.Fprintf(os.Stderr, "rmt8l: wrote %s (%d parameters)\n", txtPath, len(resp.Params))
	return exitOK
}

// pullAndWriteDevice handles the device-state half of `backup`. The radio's
// device-info response is a single `0xFF <len> <payload> <CRC8>` packet
// sandwiched in live-noise; we only persist the framed packet itself (not
// the surrounding noise) so the .bin stays byte-stable between runs of the
// same radio at the same state.
func pullAndWriteDevice(outDir, fromFile, portPath string) int {
	var raw []byte
	if fromFile != "" {
		b, err := os.ReadFile(fromFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rmt8l:", err)
			return exitGeneric
		}
		raw = b
	} else if portPath != "" {
		b, err := radio.PullDeviceInfo(portPath)
		if err != nil {
			return reportRadioErr(err)
		}
		raw = b
	} else {
		return exitOK
	}

	ds, packet, err := proto.FindDeviceInfo(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l: decode device-state:", err)
		return exitGeneric
	}

	binPath := filepath.Join(outDir, "device-state.bin")
	txtPath := filepath.Join(outDir, "device-state.txt")
	if err := os.WriteFile(binPath, packet, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}
	if err := os.WriteFile(txtPath, []byte(proto.RenderDeviceState(ds)+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}
	fmt.Fprintf(os.Stderr, "rmt8l: wrote %s (%d bytes)\n", binPath, len(packet))
	fmt.Fprintf(os.Stderr, "rmt8l: wrote %s (volume=%d, firmware=%s)\n", txtPath, ds.Volume, ds.DeviceFirmware)
	return exitOK
}

// ----- info -----

// cmdInfo prints the radio's device-state to stdout — volume, slider
// midpoint, firmware versions, serial number, etc. Default output is the
// same diff-friendly txt that `backup` writes to device-state.txt; pass
// --json for a machine-readable form.
func cmdInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	asJSON := fs.Bool("json", false, "emit JSON")
	fromFile := fs.String("from-file", "", "decode an existing device-state .bin instead of pulling fresh")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	var raw []byte
	if *fromFile != "" {
		b, err := os.ReadFile(*fromFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rmt8l:", err)
			return exitGeneric
		}
		raw = b
	} else {
		path, err := radio.Resolve(*port)
		if err != nil {
			return reportRadioErr(err)
		}
		b, err := radio.PullDeviceInfo(path)
		if err != nil {
			return reportRadioErr(err)
		}
		raw = b
	}

	ds, _, err := proto.FindDeviceInfo(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l: decode device-state:", err)
		return exitGeneric
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ds); err != nil {
			fmt.Fprintln(os.Stderr, "rmt8l:", err)
			return exitGeneric
		}
		return exitOK
	}
	fmt.Println(proto.RenderDeviceState(ds))
	return exitOK
}

// ----- ports -----

func cmdPorts(args []string) int {
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	ports, err := radio.FindAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ports)
		return exitOK
	}
	if len(ports) == 0 {
		fmt.Fprintln(os.Stderr, "no Radiomaster radios detected (is it in M+power management mode?)")
		return exitNoRadio
	}
	for _, p := range ports {
		fmt.Printf("%s\t%s\t%s:%s\t%s\n", p.Path, p.Product, p.VID, p.PID, p.Serial)
	}
	return exitOK
}

// ----- shared error mapping -----

func reportRadioErr(err error) int {
	switch {
	case errors.Is(err, radio.ErrNoRadio):
		fmt.Fprintln(os.Stderr, "rmt8l: no T8L found (is it in M+power management mode? is Chrome holding the port?)")
		return exitNoRadio
	}
	var multi *radio.ErrMultipleRadios
	if errors.As(err, &multi) {
		fmt.Fprintln(os.Stderr, "rmt8l:", multi.Error())
		fmt.Fprintln(os.Stderr, "       use --port to pick one")
		return exitMultipleRadios
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	if strings.Contains(low, "resource busy") || strings.Contains(low, "in use") {
		fmt.Fprintln(os.Stderr, "rmt8l: port already in use (quit Chrome / close the Radiomaster web configurator and retry)")
		return exitPortInUse
	}
	fmt.Fprintln(os.Stderr, "rmt8l:", err)
	return exitGeneric
}

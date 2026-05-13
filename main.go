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
  backup   Pull TX-side settings; write tx-settings.bin and tx-settings.txt
  ports    List detected Radiomaster radios
  version  Print version

Run 'rmt8l <command> --help' for command-specific flags.
`)
}

// ----- backup -----

// cmdBackup pulls the TX-side settings dump and writes two files:
//
//	<out>/tx-settings.bin  — raw response (byte-stable across runs)
//	<out>/tx-settings.txt  — decoded, diff-friendly view
//
// The .bin is the source of truth; the .txt is what `git diff` localises
// to a few lines when a setting changes.
func cmdBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	outDir := fs.String("out-dir", ".", "directory to write tx-settings.{bin,txt} into")
	fromFile := fs.String("from-file", "", "decode an existing .bin instead of pulling fresh")
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
		b, err := radio.PullTXSettings(path)
		if err != nil {
			return reportRadioErr(err)
		}
		raw = b
	}

	resp, err := proto.Decode(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l: decode:", err)
		return exitGeneric
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "rmt8l:", err)
		return exitGeneric
	}
	binPath := filepath.Join(*outDir, "tx-settings.bin")
	txtPath := filepath.Join(*outDir, "tx-settings.txt")
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

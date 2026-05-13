// Package radio handles the USB-CDC connection to a Radiomaster T8L
// transmitter in management mode. The radio enumerates as a USB-CDC ACM
// device only while the M+power combo is held at startup; in normal
// runtime mode there's no device at all.
package radio

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const (
	// Baud rate for settings I/O. The configurator uses 460800 for the
	// firmware-update path; settings transfers run at 420000.
	Baud = 420000

	// USB IDs for the T8L. The same VID/PID is likely shared with the M8L,
	// Pocket, Boxer, and TX12 variants — see Connection.html for the
	// `if (deviceName == "RadioMaster M8L")` branches that imply this.
	VendorID  = "19f5"
	ProductID = "5740"

	// CmdTXRefresh asks the radio for the full TX-side ELRS settings dump.
	// `A5 11 00 0D 0A` — subsystem 0x11, command 0x00, no args.
	CmdTXRefresh = "\xa5\x11\x00\r\n"
)

// ErrNoRadio is returned when no T8L is present on any USB serial port.
// The most common cause by far: the radio is in runtime mode (no USB
// device) rather than management mode (M held while powering on).
var ErrNoRadio = errors.New("no Radiomaster T8L found in management mode")

// ErrMultipleRadios means more than one matching radio is connected.
// The caller should ask the user to pick one via `--port`.
type ErrMultipleRadios struct{ Ports []string }

func (e *ErrMultipleRadios) Error() string {
	return "multiple Radiomaster radios found: " + strings.Join(e.Ports, ", ")
}

// Port describes a detected radio.
type Port struct {
	Path    string
	VID     string
	PID     string
	Product string
	Serial  string
}

// FindAll returns every USB serial port that looks like a T8L.
func FindAll() ([]Port, error) {
	all, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	var matches []Port
	for _, p := range all {
		if !p.IsUSB {
			continue
		}
		if !strings.EqualFold(p.VID, VendorID) || !strings.EqualFold(p.PID, ProductID) {
			continue
		}
		matches = append(matches, Port{
			Path:    p.Name,
			VID:     strings.ToLower(p.VID),
			PID:     strings.ToLower(p.PID),
			Product: p.Product,
			Serial:  p.SerialNumber,
		})
	}
	return matches, nil
}

// Resolve returns the port to use. If explicit is non-empty it is returned
// verbatim. Otherwise FindAll runs and must return exactly one match.
func Resolve(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	ports, err := FindAll()
	if err != nil {
		return "", err
	}
	switch len(ports) {
	case 0:
		return "", ErrNoRadio
	case 1:
		return ports[0].Path, nil
	default:
		paths := make([]string, len(ports))
		for i, p := range ports {
			paths[i] = p.Path
		}
		return "", &ErrMultipleRadios{Ports: paths}
	}
}

// PullTXSettings opens the port, sends A5 11 00 0D 0A, and reads bytes
// until a `0D 0A` terminator arrives or the link goes idle. Returns the
// raw response (still containing live-noise prefix bytes); decoding is
// the caller's job — keeping the transport ignorant of framing means a
// future `restore` path can reuse it without surgery.
func PullTXSettings(path string) ([]byte, error) {
	port, err := serial.Open(path, &serial.Mode{
		BaudRate: Baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = port.Close() }()

	// DTR/RTS handshake. The Python prototype does this; without it some
	// USB-CDC implementations on macOS leave the port half-open.
	_ = port.SetDTR(true)
	_ = port.SetRTS(true)
	time.Sleep(100 * time.Millisecond)

	// Drain whatever live-noise bytes are already buffered so they don't
	// front-load the response (we strip them anyway, but a shorter prefix
	// makes the sentinel search trivially fast).
	if err := drain(port, 50*time.Millisecond); err != nil {
		return nil, err
	}

	if _, err := port.Write([]byte(CmdTXRefresh)); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	out, err := readResponse(port, 3*time.Second, 400*time.Millisecond)
	if err != nil {
		return out, err
	}
	if len(out) == 0 {
		return nil, errors.New("no response — is the radio in M+power management mode?")
	}
	return out, nil
}

// readResponse reads bytes until the stream goes idle for `idle` or `total`
// elapses with no bytes at all. Stops early once we've seen a `0D 0A`
// terminator AFTER the 0x56 sentinel — the response is self-delimiting.
func readResponse(port serial.Port, total, idle time.Duration) ([]byte, error) {
	if err := port.SetReadTimeout(100 * time.Millisecond); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(total)
	var out []byte
	buf := make([]byte, 4096)
	var lastByte time.Time
	seen := false
	sentinelAt := -1
	for {
		if time.Now().After(deadline) {
			if !seen {
				return nil, errors.New("timed out waiting for radio response")
			}
			return out, nil
		}
		n, err := port.Read(buf)
		if err != nil {
			return out, err
		}
		if n > 0 {
			out = append(out, buf[:n]...)
			lastByte = time.Now()
			seen = true
			if sentinelAt < 0 {
				if i := bytes.IndexByte(out, 0x56); i >= 0 {
					sentinelAt = i
				}
			}
			// Once the sentinel is present and the buffer ends in CRLF,
			// the response is complete.
			if sentinelAt >= 0 && bytes.HasSuffix(out, []byte{'\r', '\n'}) && len(out)-sentinelAt > 4 {
				return out, nil
			}
			continue
		}
		if seen && time.Since(lastByte) >= idle {
			return out, nil
		}
	}
}

// drain reads and discards whatever bytes are buffered, returning when the
// link goes idle for `idle` or 200ms elapses without anything arriving.
func drain(port serial.Port, idle time.Duration) error {
	if err := port.SetReadTimeout(idle); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}

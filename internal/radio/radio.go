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

	// CmdRXRefresh asks the radio for the bound RX's ELRS settings.
	// `A5 22 00 0D 0A`. The radio has to poll the RX over the air, so the
	// response is slower and may be absent entirely if no RX is powered.
	CmdRXRefresh = "\xa5\x22\x00\r\n"

	// CmdDeviceInfo asks the radio for the `0xFF` device-state packet —
	// volume, slider midpoint, calibration, firmware versions, etc. The
	// configurator calls this `devAttrCmd`. The response is unframed (no
	// 0x56 sentinel, no CRLF terminator); see PullDeviceInfo.
	CmdDeviceInfo = "\xa5\x55\x1d\r\n"
)

// ErrNoRadio is returned when no T8L is present on any USB serial port.
// The most common cause by far: the radio is in runtime mode (no USB
// device) rather than management mode (M held while powering on).
var ErrNoRadio = errors.New("no Radiomaster T8L found in management mode")

// errNoBytes is the low-level signal that the radio sent absolutely nothing
// during the read window — not even live-noise. Callers translate it into
// the appropriate higher-level meaning: for TX that's "radio not in mode";
// for RX that's "no receiver bound and powered" (an expected, clean state).
var errNoBytes = errors.New("no bytes received from radio")

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

// PullTXSettings sends `A5 11 00 0D 0A` and returns the raw response. A
// `radio.errNoBytes` from pull() is translated into a user-facing hint
// because for TX it almost always means the radio is in runtime mode
// rather than management mode.
func PullTXSettings(path string) ([]byte, error) {
	out, err := pull(path, []byte(CmdTXRefresh), 3*time.Second)
	if errors.Is(err, errNoBytes) {
		return nil, errors.New("no response — is the radio in M+power management mode?")
	}
	return out, err
}

// PullRXSettings sends `A5 22 00 0D 0A` and returns the raw response. The
// radio polls the bound RX over the air, so this is slower than TX. With
// no RX bound and powered, the radio simply doesn't reply — we observed it
// going completely silent (not even the usual link/progress noise). We
// surface that as an empty success `(nil, nil)`; the caller's `proto.Decode`
// will return `proto.ErrNoPacket`, which `cmdBackup` treats as a clean skip.
// This relies on PullRXSettings only being called AFTER a TX pull has
// already proven the radio is in management mode — otherwise a silent radio
// might be misread as "no RX" when in fact the radio is off.
func PullRXSettings(path string) ([]byte, error) {
	out, err := pull(path, []byte(CmdRXRefresh), 4*time.Second)
	if errors.Is(err, errNoBytes) {
		return nil, nil
	}
	return out, err
}

// PullDeviceInfo sends `A5 55 1D 0D 0A` and returns the raw bytes received
// during the read window. Unlike TX/RX settings, the `0xFF` device-info
// response has no 0x56 sentinel and no CRLF terminator, so the read loop
// just collects bytes until the link goes idle for ~400 ms (or the total
// timeout expires). The caller is expected to feed the result through
// `proto.FindDeviceInfo`, which scans for the framed packet and validates
// the CRC8 — the radio also emits link/progress noise that can contain
// stray 0xFF bytes, so position-in-buffer alone is not enough.
func PullDeviceInfo(path string) ([]byte, error) {
	return pull(path, []byte(CmdDeviceInfo), 3*time.Second)
}

// pull opens the port, sends the given request, and reads bytes until a
// `0D 0A` terminator arrives after the `0x56` sentinel or the link goes
// idle. The transport stays ignorant of framing so a future write/restore
// path can reuse it.
func pull(path string, cmd []byte, totalTimeout time.Duration) ([]byte, error) {
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

	if _, err := port.Write(cmd); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	return readResponse(port, totalTimeout, 400*time.Millisecond)
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
				return nil, errNoBytes
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

package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// ErrNoDeviceInfo is returned by FindDeviceInfo when the buffer contains no
// valid `0xFF <len> <payload> <CRC8>` frame. Distinct from ErrNoPacket so
// callers can branch on which side of the protocol is misbehaving.
var ErrNoDeviceInfo = errors.New("no 0xFF device-info packet in response")

// DeviceState is the parsed payload of the `0xFF` device-info packet —
// volume, slider midpoint, calibration flag, firmware versions, etc. It's
// the radio's own state, separate from the chunked ELRS settings dump.
//
// Field naming mirrors parseDeviceInfoPacket in Connection.html (the
// authoritative source) so cross-referencing the configurator is easy.
type DeviceState struct {
	Volume          int    `json:"volume"`             // 0..25 in the UI (the displayed percentage is volume × 4).
	SerialNumber    string `json:"serial_number"`      // Hex string of the chip-ID bytes (trailing 0x00 padding stripped). Not the human-readable serial on the box label.
	ELRSFirmware    string `json:"elrs_firmware"`
	DeviceFirmware  string `json:"device_firmware"`
	ELRSName        string `json:"elrs_name"`
	Company         string `json:"company"`
	DeviceName      string `json:"device_name"`        // e.g. "RadioMaster T8L"
	SendPackSpeed   int    `json:"send_pack_speed"`
	ReturnSpeed     int    `json:"return_speed"`
	CalibrationFlag int    `json:"calibration_flag"`
	ConnectProtocol int    `json:"connect_protocol"`
	ChannelNum      int    `json:"channel_num"`
	SecretKey       int    `json:"secret_key"`         // The configurator parses this byte into its struct but never renders it (the HTML is commented out at Connection.html line ~9990). Captured T8L values are `0xA5`, hinting at a static magic-byte marker rather than a real key — unverified.
	PowerVal        int    `json:"power_val"`
	RemoteCtlClass  int    `json:"remote_ctl_class"`
	MixedCtl        int    `json:"mixed_ctl"`
	KeyModel        []byte `json:"key_model"`          // 6 bytes; per-button mode (single/double/long etc.)
	SliderMid       int    `json:"slider_mid"`         // 0 / 1 flag — whether the M8L-style slider midpoint override is enabled.
	SliderMidData   int    `json:"slider_mid_data"`    // 16-bit ADC count for the slider midpoint.
	RxWarning       int    `json:"rx_warning"`
	LowPowerWarning int    `json:"low_power_warning"`
	LeftTrimReset   int    `json:"left_trim_reset"`
	RightTrimReset  int    `json:"right_trim_reset"`
}

// FindDeviceInfo scans raw for a valid `0xFF <len> <payload> <CRC8>` frame,
// validates the CRC, parses the payload, and returns the DeviceState along
// with the extracted packet bytes (header + payload + CRC = len+2 bytes).
//
// The radio interleaves device-info responses with link/progress noise that
// can itself contain 0xFF bytes, so we can't just take the first 0xFF —
// CRC validation is the disambiguator.
func FindDeviceInfo(raw []byte) (DeviceState, []byte, error) {
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] != 0xFF {
			continue
		}
		l := int(raw[i+1])
		if l < 2 {
			continue
		}
		end := i + l + 2
		if end > len(raw) {
			continue
		}
		// CRC byte sits at end-1; it's computed over raw[i+2 : end-1]
		// (len-1 bytes — the entire payload minus the CRC itself).
		if crc8(raw[i+2:end-1]) != raw[end-1] {
			continue
		}
		ds, err := parseDeviceState(raw[i+2 : end-1])
		if err != nil {
			continue
		}
		return ds, raw[i:end], nil
	}
	return DeviceState{}, nil, ErrNoDeviceInfo
}

// parseDeviceState decodes the validated payload (everything between the
// 0xFF/len header and the trailing CRC). T8L payloads are 163 bytes;
// longer payloads (e.g. M8L with extra slider ADC data) are accepted — we
// just read the T8L-relevant fields and ignore the rest.
func parseDeviceState(payload []byte) (DeviceState, error) {
	// 141 bytes for the string block + 22 bytes of trailing fields up to
	// RightTrimReset = 163 bytes minimum for a T8L payload.
	if len(payload) < 163 {
		return DeviceState{}, fmt.Errorf("device-info payload too short (%dB, want >=163)", len(payload))
	}
	cstr := func(b []byte) string {
		if n := bytes.IndexByte(b, 0); n >= 0 {
			return string(b[:n])
		}
		return string(b)
	}
	// Strings, in JS-prototype order. Offsets here are 0-based into payload
	// (i.e. JS's `buf[2..]` is our `payload[0..]`).
	d := DeviceState{
		Volume:         int(payload[0]),
		SerialNumber:   hexStripTrailingZero(payload[1:21]),
		ELRSFirmware:   cstr(payload[21:41]),
		DeviceFirmware: cstr(payload[41:51]),
		ELRSName:       cstr(payload[51:101]),
		Company:        cstr(payload[101:121]),
		DeviceName:     cstr(payload[121:141]),
	}
	// Trailing fields. JS reads these via `offset = 143` (which is our
	// `o = 141` in 0-based payload coords).
	o := 141
	d.SendPackSpeed = int(payload[o+0])
	d.ReturnSpeed = int(payload[o+1])
	d.CalibrationFlag = int(payload[o+2])
	d.ConnectProtocol = int(payload[o+3])
	d.ChannelNum = int(payload[o+4])
	d.SecretKey = int(payload[o+5])
	d.PowerVal = int(payload[o+6])
	d.RemoteCtlClass = int(payload[o+7])
	d.MixedCtl = int(payload[o+8])
	d.KeyModel = append([]byte(nil), payload[o+9:o+15]...)
	d.SliderMid = int(payload[o+15])
	d.SliderMidData = int(binary.LittleEndian.Uint16(payload[o+16 : o+18]))
	d.RxWarning = int(payload[o+18])
	d.LowPowerWarning = int(payload[o+19])
	d.LeftTrimReset = int(payload[o+20])
	d.RightTrimReset = int(payload[o+21])
	return d, nil
}

// RenderDeviceState produces the diff-friendly txt for a DeviceState —
// same shape as the TX render: a `device:` header block, then a flat
// `settings:` list ordered by logical grouping rather than wire order so
// the most "user-visible" fields (volume, slider) are near the top.
func RenderDeviceState(d DeviceState) string {
	var b strings.Builder
	b.WriteString("device:\n")
	fmt.Fprintf(&b, "  name:            %s\n", d.DeviceName)
	fmt.Fprintf(&b, "  serial:          %s\n", d.SerialNumber)
	fmt.Fprintf(&b, "  elrs_firmware:   %s\n", d.ELRSFirmware)
	fmt.Fprintf(&b, "  device_firmware: %s\n", d.DeviceFirmware)
	fmt.Fprintf(&b, "  elrs_name:       %s\n", d.ELRSName)
	fmt.Fprintf(&b, "  company:         %s\n", d.Company)
	b.WriteString("\n")
	b.WriteString("settings:\n")
	fmt.Fprintf(&b, "  volume:            %d # %d%%\n", d.Volume, d.Volume*4)
	fmt.Fprintf(&b, "  slider_mid:        %d\n", d.SliderMid)
	fmt.Fprintf(&b, "  slider_mid_data:   %d\n", d.SliderMidData)
	fmt.Fprintf(&b, "  calibration_flag:  %d\n", d.CalibrationFlag)
	fmt.Fprintf(&b, "  channel_num:       %d\n", d.ChannelNum)
	fmt.Fprintf(&b, "  power_val:         %d\n", d.PowerVal)
	fmt.Fprintf(&b, "  send_pack_speed:   %d\n", d.SendPackSpeed)
	fmt.Fprintf(&b, "  return_speed:      %d\n", d.ReturnSpeed)
	fmt.Fprintf(&b, "  connect_protocol:  %d\n", d.ConnectProtocol)
	fmt.Fprintf(&b, "  secret_key:        %d\n", d.SecretKey)
	fmt.Fprintf(&b, "  remote_ctl_class:  %d\n", d.RemoteCtlClass)
	fmt.Fprintf(&b, "  mixed_ctl:         %d\n", d.MixedCtl)
	fmt.Fprintf(&b, "  rx_warning:        %d\n", d.RxWarning)
	fmt.Fprintf(&b, "  low_power_warning: %d\n", d.LowPowerWarning)
	fmt.Fprintf(&b, "  left_trim_reset:   %d\n", d.LeftTrimReset)
	fmt.Fprintf(&b, "  right_trim_reset:  %d\n", d.RightTrimReset)
	fmt.Fprintf(&b, "  key_model:         %s\n", hexBytes(d.KeyModel))
	// Expanded per-switch view: T8L assigns positions 0..4 to switches SA..SE,
	// with position 5 always a fixed `0x07` terminator. Each byte is a mode
	// enum from the configurator dropdown (see Connection.html ~lines 9739–9743).
	// Show both raw byte and decoded label per line so a switch-type change
	// localises the diff to one line.
	keyLabels := [6]string{"SA", "SB", "SC", "SD", "SE", "--"}
	for i := 0; i < 6 && i < len(d.KeyModel); i++ {
		v := d.KeyModel[i]
		var name string
		switch {
		case i == 5 && v == 0x07:
			name = "terminator"
		default:
			if n, ok := keyModeNames[v]; ok {
				name = n
			} else {
				name = "unknown"
			}
		}
		fmt.Fprintf(&b, "    %s: [%02x] %s\n", keyLabels[i], v, name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// keyModeNames maps the T8L key-mode enum (the bytes stored in KeyModel[0..4])
// to the exact English labels the web configurator shows on the switch
// dropdowns. Source: the en-US LANGS table in Connection.html (~lines
// 11681–11685). Matching the configurator's wording lets a user cross-check
// a `device-state.txt` against the UI by visual inspection — note in
// particular that `0x06` is shown as "Click" (not the literal translation
// of 触发, "trigger") and `0x03` is shown as "Single" (single-click).
var keyModeNames = map[byte]string{
	0x01: "2-POS",
	0x02: "Double",
	0x03: "Single",
	0x04: "3-POS",
	0x06: "Click",
}

func hexBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(parts, " ")
}

// hexStripTrailingZero hex-encodes b and strips trailing `00` padding.
// Matches the web configurator's serial-number rendering: the underlying
// field is a fixed-width byte buffer that's only partially filled, so the
// trailing zeros are meaningless padding rather than data.
func hexStripTrailingZero(b []byte) string {
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	parts := make([]string, end)
	for i := 0; i < end; i++ {
		parts[i] = fmt.Sprintf("%02x", b[i])
	}
	return strings.Join(parts, "")
}

// crc8tab is the Radiomaster lookup table for the CRC8 over device-info and
// most subsystem-0x55 responses. Copied verbatim from `crc8tab_js` in
// Connection.html — the polynomial is implicit in the table; do not try to
// regenerate it from first principles.
var crc8tab = [256]byte{
	0x00, 0xD5, 0x7F, 0xAA, 0xFE, 0x2B, 0x81, 0x54, 0x29, 0xFC, 0x56, 0x83, 0xD7, 0x02, 0xA8, 0x7D,
	0x52, 0x87, 0x2D, 0xF8, 0xAC, 0x79, 0xD3, 0x06, 0x7B, 0xAE, 0x04, 0xD1, 0x85, 0x50, 0xFA, 0x2F,
	0xA4, 0x71, 0xDB, 0x0E, 0x5A, 0x8F, 0x25, 0xF0, 0x8D, 0x58, 0xF2, 0x27, 0x73, 0xA6, 0x0C, 0xD9,
	0xF6, 0x23, 0x89, 0x5C, 0x08, 0xDD, 0x77, 0xA2, 0xDF, 0x0A, 0xA0, 0x75, 0x21, 0xF4, 0x5E, 0x8B,
	0x9D, 0x48, 0xE2, 0x37, 0x63, 0xB6, 0x1C, 0xC9, 0xB4, 0x61, 0xCB, 0x1E, 0x4A, 0x9F, 0x35, 0xE0,
	0xCF, 0x1A, 0xB0, 0x65, 0x31, 0xE4, 0x4E, 0x9B, 0xE6, 0x33, 0x99, 0x4C, 0x18, 0xCD, 0x67, 0xB2,
	0x39, 0xEC, 0x46, 0x93, 0xC7, 0x12, 0xB8, 0x6D, 0x10, 0xC5, 0x6F, 0xBA, 0xEE, 0x3B, 0x91, 0x44,
	0x6B, 0xBE, 0x14, 0xC1, 0x95, 0x40, 0xEA, 0x3F, 0x42, 0x97, 0x3D, 0xE8, 0xBC, 0x69, 0xC3, 0x16,
	0xEF, 0x3A, 0x90, 0x45, 0x11, 0xC4, 0x6E, 0xBB, 0xC6, 0x13, 0xB9, 0x6C, 0x38, 0xED, 0x47, 0x92,
	0xBD, 0x68, 0xC2, 0x17, 0x43, 0x96, 0x3C, 0xE9, 0x94, 0x41, 0xEB, 0x3E, 0x6A, 0xBF, 0x15, 0xC0,
	0x4B, 0x9E, 0x34, 0xE1, 0xB5, 0x60, 0xCA, 0x1F, 0x62, 0xB7, 0x1D, 0xC8, 0x9C, 0x49, 0xE3, 0x36,
	0x19, 0xCC, 0x66, 0xB3, 0xE7, 0x32, 0x98, 0x4D, 0x30, 0xE5, 0x4F, 0x9A, 0xCE, 0x1B, 0xB1, 0x64,
	0x72, 0xA7, 0x0D, 0xD8, 0x8C, 0x59, 0xF3, 0x26, 0x5B, 0x8E, 0x24, 0xF1, 0xA5, 0x70, 0xDA, 0x0F,
	0x20, 0xF5, 0x5F, 0x8A, 0xDE, 0x0B, 0xA1, 0x74, 0x09, 0xDC, 0x76, 0xA3, 0xF7, 0x22, 0x88, 0x5D,
	0xD6, 0x03, 0xA9, 0x7C, 0x28, 0xFD, 0x57, 0x82, 0xFF, 0x2A, 0x80, 0x55, 0x01, 0xD4, 0x7E, 0xAB,
	0x84, 0x51, 0xFB, 0x2E, 0x7A, 0xAF, 0x05, 0xD0, 0xAD, 0x78, 0xD2, 0x07, 0x53, 0x86, 0x2C, 0xF9,
}

func crc8(data []byte) byte {
	var c byte
	for _, b := range data {
		c = crc8tab[c^b]
	}
	return c
}

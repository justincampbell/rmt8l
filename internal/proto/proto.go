// Package proto decodes the Radiomaster T8L "TX-side settings" response
// (A5 11 00 0D 0A → 0x56 …). It's a faithful port of the Python prototype
// at fpv/scripts/t8l-backup.py — the goal is a byte-identical render against
// the fixture in testdata/, not a redesign.
//
// Wire format (reverse-engineered from radiomaster-rc/RM-Web-Page Connection.html):
//
//	<noise> 56 <type:1> <prefix:1> <chunk…> <chunk…> <body-CRC:2> 0D 0A
//
// where each chunk is
//
//	EA <len> <marker> EA EE <payload[len-4]> <CRC8>
//
// Markers: 0x29 = device info; 0x2B = parameter record (one setting, possibly
// split across multiple chunks). Parameter chunks for one record share the
// `pid` byte and use a `seq` byte counting down to 0; payloads concatenate in
// arrival order.
//
// Parameter body (after merging chunks):
//
//	parent:u8  dataType:u8  name\0  <type-specific fields>
//
// dataType: low 7 bits = base type, high bit = "hidden" (live counter) flag.
package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Base parameter types — values match parseParamData in Connection.html.
const (
	TypeUint8     = 0
	TypeInt8      = 1
	TypeUint16    = 2
	TypeInt16     = 3
	TypeFloat     = 8
	TypeSelection = 9
	TypeString    = 10
	TypeFolder    = 11
	TypeInfo      = 12
	TypeCommand   = 13
)

// TypeNames maps base type to the short label used in the diff-friendly txt.
var TypeNames = map[int]string{
	TypeUint8:     "uint8",
	TypeInt8:      "int8",
	TypeUint16:    "uint16",
	TypeInt16:     "int16",
	TypeFloat:     "float",
	TypeSelection: "selection",
	TypeString:    "string",
	TypeFolder:    "folder",
	TypeInfo:      "info",
	TypeCommand:   "command",
}

// Chunk markers.
const (
	markerDeviceInfo = 0x29
	markerParameter  = 0x2B
)

// Sentinel that marks the start of a real response inside the live-noise
// byte stream the radio emits while idle.
const sentinel = 0x56

// Chunk is one decoded chunk from the response body.
type Chunk struct {
	Marker  byte
	Payload []byte
	CRC8    byte
}

// DeviceInfo captures the parsed `0x29` chunk: a concatenation of
// null-terminated strings (model, module, …) followed by some unidentified
// trailing bytes. We extract what the prototype extracts and stash the raw
// trailing tail for diffing.
type DeviceInfo struct {
	Strings           []string
	RawTailHex        string
	LikelyNumSettings int // 0 if no plausible candidate
}

// Param is one decoded setting. Fields not relevant to a given base type are
// left at their zero value; the renderer keys off Type to know which to print.
type Param struct {
	ID     int
	Parent int
	Type   int
	Hidden bool // live-updating counter; excluded from diffs in practice
	Name   string

	// Numeric (uint8/int8/uint16/int16). Signed types use the same fields,
	// already sign-extended.
	Value   int
	Min     int
	Max     int
	Default int
	Unit    string

	// Selection: Options is the parsed list, Current is Options[Value].
	Options []string
	Current string

	// String.
	MaxLen      int
	StringValue string
	StringDef   string

	// Info / Command.
	InfoValue string
	Status    int
	Timeout   int
	CmdInfo   string

	// Error (set if the chunk body was malformed). When non-empty the
	// renderer surfaces it so a regression is visible in the diff.
	Err    string
	RawHex string
}

// Response bundles a decoded `A5 11` reply.
type Response struct {
	Device DeviceInfo
	Params []Param
}

// Decode is the end-to-end happy path: raw radio bytes → Response.
func Decode(raw []byte) (Response, error) {
	body, _, err := FindPacket(raw)
	if err != nil {
		return Response{}, err
	}
	chunks := WalkChunks(body)

	var deviceChunk *Chunk
	for i := range chunks {
		if chunks[i].Marker == markerDeviceInfo {
			deviceChunk = &chunks[i]
			break
		}
	}
	if deviceChunk == nil {
		return Response{}, errors.New("no device-info chunk (0x29) found")
	}
	dev := ParseDeviceInfo(deviceChunk.Payload)

	merged := MergeChunks(chunks)
	params := make([]Param, 0, len(merged))
	for _, m := range merged {
		p := ParseParam(m.Body)
		p.ID = int(m.PID)
		params = append(params, p)
	}
	return Response{Device: dev, Params: params}, nil
}

// FindPacket skips live-traffic noise (0x67 link, 0x23 progress, …) until
// it finds the 0x56 sentinel, then returns the body (between sentinel and
// trailing CRC+CRLF) along with the 2-byte body CRC.
//
// The body CRC algorithm is undocumented and not validated here — the
// prototype doesn't validate it either, and rejecting valid dumps for the
// wrong CRC formula would be worse than skipping the check.
func FindPacket(raw []byte) (body, bodyCRC []byte, err error) {
	i := bytes.IndexByte(raw, sentinel)
	if i < 0 {
		return nil, nil, errors.New("no 0x56 sentinel in response")
	}
	if !bytes.HasSuffix(raw, []byte{'\r', '\n'}) {
		return nil, nil, fmt.Errorf("response missing CRLF terminator (tail: %x)", trailing(raw, 6))
	}
	if len(raw)-i-1 < 4 {
		return nil, nil, errors.New("response truncated after sentinel")
	}
	body = raw[i+1 : len(raw)-4]
	bodyCRC = raw[len(raw)-4 : len(raw)-2]
	return body, bodyCRC, nil
}

func trailing(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

// WalkChunks scans body and yields one Chunk per `EA len marker EA EE …`
// frame. Body layout:
//
//	<type:1> <prefix:1> EA len marker EA EE payload[len-4] CRC8 EA …
//
// The first two bytes are a packet-level prefix (type byte, always 0x04 in
// observed dumps, and a second byte of unclear semantics). After that,
// chunks pack back-to-back: each one's CRC8 sits immediately before the
// next one's leading EA. We scan for the EA…EA EE marker pattern rather
// than hard-coding the two-byte prefix length, so an off-by-one or
// firmware variation that adds another prefix byte doesn't break the walker.
//
// One quirk: in the fixture the last chunk's payload runs to the very last
// byte of the body, with its CRC8 logically belonging *after* the body
// boundary (it sits in the bytes Python's `find_packet` treats as the body
// CRC). We tolerate that by emitting the chunk with CRC8=0 when the payload
// ends exactly at len(body); a truncated payload still aborts the walk.
func WalkChunks(body []byte) []Chunk {
	var out []Chunk
	i := 0
	for i+5 <= len(body) {
		if body[i] != 0xEA || body[i+3] != 0xEA || body[i+4] != 0xEE {
			i++
			continue
		}
		length := int(body[i+1])
		marker := body[i+2]
		payloadStart := i + 5
		payloadEnd := payloadStart + length - 4
		if payloadEnd > len(body) {
			break // truncated payload — bail out rather than panic
		}
		var crc byte
		if payloadEnd < len(body) {
			crc = body[payloadEnd]
		}
		out = append(out, Chunk{
			Marker:  marker,
			Payload: body[payloadStart:payloadEnd],
			CRC8:    crc,
		})
		i = payloadEnd + 1
	}
	return out
}

// ParseDeviceInfo splits the 0x29 chunk payload into its null-terminated
// string components and pulls the "likely number of settings" heuristic the
// prototype uses (last non-zero byte of the trailing metadata).
func ParseDeviceInfo(payload []byte) DeviceInfo {
	parts := bytes.Split(payload, []byte{0})
	var strs []string
	for _, p := range parts {
		if len(p) > 0 {
			strs = append(strs, string(p))
		}
	}
	info := DeviceInfo{Strings: strs}

	tail := payload
	if len(tail) > 9 {
		tail = tail[len(tail)-9:]
	}
	info.RawTailHex = fmt.Sprintf("%x", tail)

	scan := payload
	if len(scan) > 12 {
		scan = scan[len(scan)-12:]
	}
	for i := len(scan) - 1; i >= 0; i-- {
		if scan[i] != 0 {
			info.LikelyNumSettings = int(scan[i])
			break
		}
	}
	return info
}

// MergedRecord is a parameter's full body after gluing its chunks together.
type MergedRecord struct {
	PID      byte
	FirstSeq byte // seq value of the first chunk seen (highest, since seq counts down)
	Body     []byte
}

// MergeChunks concatenates parameter chunks that share a pid. Chunks for a
// single parameter use decreasing seq → 0; arrival order is also seq order,
// so we just append payloads after stripping the (pid, seq) header.
func MergeChunks(chunks []Chunk) []MergedRecord {
	type accum struct {
		firstSeq byte
		body     []byte
	}
	records := make(map[byte]*accum)
	var order []byte
	for _, c := range chunks {
		if c.Marker != markerParameter || len(c.Payload) < 2 {
			continue
		}
		pid := c.Payload[0]
		seq := c.Payload[1]
		rest := c.Payload[2:]
		if a, ok := records[pid]; ok {
			a.body = append(a.body, rest...)
		} else {
			records[pid] = &accum{firstSeq: seq, body: append([]byte(nil), rest...)}
			order = append(order, pid)
		}
	}
	out := make([]MergedRecord, 0, len(order))
	for _, pid := range order {
		a := records[pid]
		out = append(out, MergedRecord{PID: pid, FirstSeq: a.firstSeq, Body: a.body})
	}
	return out
}

// ParseParam decodes one merged parameter body. It mirrors parseParamData
// from Connection.html (also documented in the Python prototype's docstring).
func ParseParam(body []byte) Param {
	if len(body) < 3 {
		return Param{Err: fmt.Sprintf("short body (%dB): %x", len(body), body)}
	}
	parent := body[0]
	dataType := body[1]
	baseType := int(dataType & 0x7F)
	hidden := dataType&0x80 != 0

	r := reader{buf: body, pos: 2}
	name := r.cstr()
	p := Param{Parent: int(parent), Type: baseType, Hidden: hidden, Name: name}

	switch baseType {
	case TypeUint8, TypeInt8:
		if !r.has(4) {
			p.Err = "truncated u8/i8 trailer"
			return p
		}
		p.Value = signExtend8(r.u8(), baseType == TypeInt8)
		p.Min = signExtend8(r.u8(), baseType == TypeInt8)
		p.Max = signExtend8(r.u8(), baseType == TypeInt8)
		p.Default = signExtend8(r.u8(), baseType == TypeInt8)
		p.Unit = r.cstr()

	case TypeUint16, TypeInt16:
		if !r.has(8) {
			p.Err = "truncated u16/i16 trailer"
			return p
		}
		p.Value = signExtend16(r.u16(), baseType == TypeInt16)
		p.Min = signExtend16(r.u16(), baseType == TypeInt16)
		p.Max = signExtend16(r.u16(), baseType == TypeInt16)
		p.Default = signExtend16(r.u16(), baseType == TypeInt16)
		p.Unit = r.cstr()

	case TypeSelection:
		optsEnd := bytes.IndexByte(r.rest(), 0)
		if optsEnd < 0 {
			p.Err = "no options terminator"
			return p
		}
		p.Options = decodeOptions(r.take(optsEnd))
		r.skip(1) // consume the \0
		if !r.has(4) {
			p.Err = "truncated selection trailer"
			return p
		}
		p.Value = int(r.u8())
		p.Min = int(r.u8())
		p.Max = int(r.u8())
		p.Default = int(r.u8())
		p.Unit = r.cstr()
		if p.Value >= 0 && p.Value < len(p.Options) {
			p.Current = p.Options[p.Value]
		} else {
			p.Current = fmt.Sprintf("<idx %d>", p.Value)
		}

	case TypeString:
		if !r.has(1) {
			p.Err = "truncated string header"
			return p
		}
		p.MaxLen = int(r.u8())
		p.StringValue = r.cstr()
		p.StringDef = r.cstr()

	case TypeFolder:
		// Name IS the section heading; no further payload.

	case TypeInfo:
		p.InfoValue = r.cstr()

	case TypeCommand:
		if !r.has(2) {
			p.Err = "truncated command header"
			return p
		}
		p.Status = int(r.u8())
		p.Timeout = int(r.u8())
		p.CmdInfo = r.cstr()

	default:
		p.Err = fmt.Sprintf("unhandled type %d", baseType)
		p.RawHex = fmt.Sprintf("%x", r.rest())
	}
	return p
}

func signExtend8(b byte, signed bool) int {
	if signed {
		return int(int8(b))
	}
	return int(b)
}

func signExtend16(u uint16, signed bool) int {
	if signed {
		return int(int16(u))
	}
	return int(u)
}

// decodeOptions splits a `;`-separated TEXT_SELECTION option list, replacing
// Radiomaster's special pitmode markers (\xC0 → "↓", \xC1 → "↑") and quoting
// anything else outside printable ASCII as `\xNN`. Matches the prototype's
// decode_options.
func decodeOptions(raw []byte) []string {
	pieces := bytes.Split(raw, []byte{';'})
	out := make([]string, 0, len(pieces))
	for _, piece := range pieces {
		var b strings.Builder
		for _, c := range piece {
			switch {
			case c == 0xC0:
				b.WriteString("↓")
			case c == 0xC1:
				b.WriteString("↑")
			case c >= 32 && c < 127:
				b.WriteByte(c)
			default:
				fmt.Fprintf(&b, `\x%02x`, c)
			}
		}
		out = append(out, b.String())
	}
	return out
}

// reader is a small forward-only cursor over a byte slice with the read
// primitives the param parser needs.
type reader struct {
	buf []byte
	pos int
}

func (r *reader) has(n int) bool   { return r.pos+n <= len(r.buf) }
func (r *reader) rest() []byte     { return r.buf[r.pos:] }
func (r *reader) skip(n int)       { r.pos += n }
func (r *reader) take(n int) []byte {
	end := r.pos + n
	if end > len(r.buf) {
		end = len(r.buf)
	}
	s := r.buf[r.pos:end]
	r.pos = end
	return s
}

func (r *reader) u8() byte {
	if r.pos >= len(r.buf) {
		return 0
	}
	v := r.buf[r.pos]
	r.pos++
	return v
}

func (r *reader) u16() uint16 {
	if r.pos+2 > len(r.buf) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.buf[r.pos:])
	r.pos += 2
	return v
}

func (r *reader) cstr() string {
	end := bytes.IndexByte(r.buf[r.pos:], 0)
	if end < 0 {
		s := string(r.buf[r.pos:])
		r.pos = len(r.buf)
		return s
	}
	s := string(r.buf[r.pos : r.pos+end])
	r.pos += end + 1
	return s
}

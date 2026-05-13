package proto

import (
	"fmt"
	"strings"
)

// Render produces the diff-friendly txt that goes next to the .bin.
// Output is byte-for-byte compatible with the Python prototype's render()
// (see testdata/tx-settings.txt for the canonical form).
func Render(r Response) string {
	var b strings.Builder

	radio := "?"
	module := "?"
	if len(r.Device.Strings) > 0 {
		radio = r.Device.Strings[0]
	}
	if len(r.Device.Strings) > 1 {
		module = r.Device.Strings[1]
	}
	fmt.Fprintf(&b, "radio: %s\n", radio)
	fmt.Fprintf(&b, "module: %s\n", module)
	if r.Device.LikelyNumSettings != 0 {
		fmt.Fprintf(&b, "settings_count: %d\n", r.Device.LikelyNumSettings)
	}
	b.WriteString("\n")
	b.WriteString("settings:\n")

	for _, p := range r.Params {
		label, ok := TypeNames[p.Type]
		if !ok {
			label = fmt.Sprintf("type=%d", p.Type)
		}
		var flags string
		switch {
		case p.Type == TypeInfo && p.Hidden:
			flags = " (live)"
		case p.Hidden:
			flags = " (hidden)"
		}
		fmt.Fprintf(&b, "  - id: %02d  parent: %02d  type: %s%s\n", p.ID, p.Parent, label, flags)
		fmt.Fprintf(&b, "    name: %s\n", p.Name)

		switch p.Type {
		case TypeUint8, TypeInt8, TypeUint16, TypeInt16:
			fmt.Fprintf(&b, "    value:   %d%s\n", p.Value, formatUnit(p.Unit))
			fmt.Fprintf(&b, "    range:   %d..%d\n", p.Min, p.Max)
			fmt.Fprintf(&b, "    default: %d\n", p.Default)

		case TypeSelection:
			current := p.Current
			if current == "" {
				current = "?"
			}
			fmt.Fprintf(&b, "    value:   [%d] %s%s\n", p.Value, current, formatUnit(p.Unit))
			fmt.Fprintf(&b, "    default: [%d]\n", p.Default)
			b.WriteString("    options:\n")
			for i, opt := range p.Options {
				marker := " "
				if i == p.Value {
					marker = "*"
				}
				fmt.Fprintf(&b, "      %s [%d] %s\n", marker, i, opt)
			}

		case TypeString:
			fmt.Fprintf(&b, "    value:   %s\n", quotePythonRepr(p.StringValue))
			fmt.Fprintf(&b, "    default: %s\n", quotePythonRepr(p.StringDef))
			fmt.Fprintf(&b, "    max_len: %d\n", p.MaxLen)

		case TypeFolder:
			// name is the heading; nothing else to emit

		case TypeInfo:
			fmt.Fprintf(&b, "    value:   %s\n", p.InfoValue)

		case TypeCommand:
			fmt.Fprintf(&b, "    status:  %d\n", p.Status)
			fmt.Fprintf(&b, "    timeout: %d\n", p.Timeout)
			if p.CmdInfo != "" {
				fmt.Fprintf(&b, "    info:    %s\n", p.CmdInfo)
			}
		}

		if p.Err != "" {
			fmt.Fprintf(&b, "    error: %s\n", p.Err)
			if p.RawHex != "" {
				fmt.Fprintf(&b, "    raw:   %s\n", p.RawHex)
			}
		}
		b.WriteString("\n")
	}

	// The Python rendered text ends with the trailing blank line from the
	// per-record loop and no extra newline beyond that. strings.Join with
	// "\n" on the prototype's list produces the same final blank line — be
	// careful not to add an extra one here.
	out := b.String()
	out = strings.TrimRight(out, "\n")
	return out
}

func formatUnit(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}

// quotePythonRepr mimics Python's `repr()` for the narrow subset of strings
// the T8L emits in STRING fields (model name, etc.): single quotes around an
// ASCII-printable string with no internal escapes. Used so the txt matches
// the prototype's `value:   {p['value']!r}` line.
func quotePythonRepr(s string) string {
	return "'" + s + "'"
}

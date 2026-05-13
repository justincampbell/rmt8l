package proto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenDecode is the load-bearing test: feed the fixture .bin through
// the full Decode → Render pipeline and assert it matches the committed .txt
// byte-for-byte. The fixture is a real 1162-byte dump from a T8L; once this
// passes, live pulls are mechanical.
func TestGoldenDecode(t *testing.T) {
	rawPath := filepath.Join("testdata", "tx-settings.bin")
	wantPath := filepath.Join("testdata", "tx-settings.txt")

	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}

	resp, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	got := Render(resp)
	wantStr := strings.TrimRight(string(want), "\n")
	if got != wantStr {
		// Surface the first divergence so the failure is localisable.
		gl := strings.Split(got, "\n")
		wl := strings.Split(wantStr, "\n")
		n := len(gl)
		if len(wl) < n {
			n = len(wl)
		}
		for i := 0; i < n; i++ {
			if gl[i] != wl[i] {
				t.Fatalf("first divergence at line %d:\n  got:  %q\n  want: %q",
					i+1, gl[i], wl[i])
			}
		}
		if len(gl) != len(wl) {
			t.Fatalf("line count mismatch: got %d, want %d", len(gl), len(wl))
		}
		t.Fatal("Render output differs (no line-level diff found)")
	}
}

func TestFindPacket(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "tx-settings.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	body, crc, err := FindPacket(raw)
	if err != nil {
		t.Fatalf("FindPacket: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("empty body")
	}
	if len(crc) != 2 {
		t.Fatalf("body CRC: want 2 bytes, got %d", len(crc))
	}
}

func TestFindPacketRejectsMissingSentinel(t *testing.T) {
	_, _, err := FindPacket([]byte{0x00, 0x01, 0x02, '\r', '\n'})
	if err == nil {
		t.Fatal("expected error for missing sentinel")
	}
	if !errors.Is(err, ErrNoPacket) {
		t.Fatalf("expected ErrNoPacket, got %v", err)
	}
}

// TestDecodeUnframedReturnsErrNoPacket simulates the RX-empty case: the
// radio is in management mode but no RX is bound, so the captured bytes
// contain only live-noise (link / progress packets) and no 0x56 sentinel.
// Decode must surface ErrNoPacket so the backup command can branch cleanly.
func TestDecodeUnframedReturnsErrNoPacket(t *testing.T) {
	// 14-byte 0x67 link packet + 5-byte 0x23 progress packet; no 0x56.
	noise := []byte{
		0x67, 0x0c, 0x14, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0x23, 0x03, 0x00, 0x00, 0x00,
	}
	_, err := Decode(noise)
	if !errors.Is(err, ErrNoPacket) {
		t.Fatalf("expected ErrNoPacket, got %v", err)
	}
}

func TestFindPacketRejectsMissingCRLF(t *testing.T) {
	_, _, err := FindPacket([]byte{sentinel, 0x04, 0xEA})
	if err == nil {
		t.Fatal("expected error for missing CRLF terminator")
	}
}

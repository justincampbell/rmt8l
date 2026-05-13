package proto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenDeviceInfo: feed the captured .bin through FindDeviceInfo +
// RenderDeviceState and assert it matches the committed .txt byte-for-byte.
// Same shape as TestGoldenDecode but for the 0xFF device-info path.
func TestGoldenDeviceInfo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "device-state.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "device-state.txt"))
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}

	ds, packet, err := FindDeviceInfo(raw)
	if err != nil {
		t.Fatalf("FindDeviceInfo: %v", err)
	}
	if len(packet) != len(raw) {
		// The fixture .bin is the extracted packet itself (no surrounding
		// noise), so packet should equal raw.
		t.Fatalf("packet length: got %d, want %d", len(packet), len(raw))
	}

	got := RenderDeviceState(ds)
	wantStr := strings.TrimRight(string(want), "\n")
	if got != wantStr {
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
		t.Fatal("RenderDeviceState output differs (no line-level diff found)")
	}
}

// TestFindDeviceInfoInNoise: pad the fixture with the same kind of live-noise
// the radio interleaves (a couple of 0x67 link-strength packets) and ensure
// FindDeviceInfo still locates and validates the real packet. Defends against
// regressions in the scan loop that could be tempted to bail on the first
// 0xFF byte it sees, even when it's just noise.
func TestFindDeviceInfoInNoise(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "device-state.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Prefix with a 0x67 link packet (whose payload is mostly 0xFF) and a
	// short 0x23 progress packet — the two unsolicited streams the radio
	// emits continuously while idle.
	prefix := []byte{
		0x67, 0x0c, 0x14, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0x23, 0x03, 0x00, 0x00, 0x00,
	}
	mixed := append(prefix, raw...)
	mixed = append(mixed, 0x67, 0x0c) // trailing partial noise too

	ds, _, err := FindDeviceInfo(mixed)
	if err != nil {
		t.Fatalf("FindDeviceInfo: %v", err)
	}
	if ds.DeviceName == "" {
		t.Fatal("DeviceName empty — scan likely matched the wrong byte")
	}
}

// TestFindDeviceInfoRejectsBadCRC: corrupt the CRC byte and ensure
// FindDeviceInfo refuses to return the packet. This is the critical
// defence against false matches in live-noise; without CRC validation
// we'd happily decode random bytes as if they were a device-state packet.
func TestFindDeviceInfoRejectsBadCRC(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "device-state.bin"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	corrupt := append([]byte(nil), raw...)
	corrupt[len(corrupt)-1] ^= 0xFF
	_, _, err = FindDeviceInfo(corrupt)
	if !errors.Is(err, ErrNoDeviceInfo) {
		t.Fatalf("expected ErrNoDeviceInfo for corrupt CRC, got %v", err)
	}
}

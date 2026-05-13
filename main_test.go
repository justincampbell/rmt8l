package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupFromFile exercises the full backup pipeline against the fixture
// .bin without touching a real radio — it builds rmt8l, runs `rmt8l backup
// --from-file <fixture> --out-dir <tmp>`, and asserts both output files are
// present and sane. This is the closest thing to an end-to-end test we get
// without hardware.
func TestBackupFromFile(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "rmt8l")

	// Build the binary fresh so the test fails if main.go stops compiling.
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	fixture, err := filepath.Abs(filepath.Join("internal", "proto", "testdata", "tx-settings.bin"))
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tmp, "out")

	run := exec.Command(bin, "backup", "--from-file", fixture, "--out-dir", outDir)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("backup failed: %v\n%s", err, out)
	}

	binOut, err := os.ReadFile(filepath.Join(outDir, "tx-settings.bin"))
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	if len(binOut) != 1162 {
		t.Errorf("tx-settings.bin: want 1162 bytes, got %d", len(binOut))
	}

	txt, err := os.ReadFile(filepath.Join(outDir, "tx-settings.txt"))
	if err != nil {
		t.Fatalf("read txt: %v", err)
	}
	s := string(txt)
	for _, want := range []string{
		"radio: RM T8L",
		"module: ELRS",
		"name: Packet Rate",
		"name: Bind",
		"d84313", // bind UID — confirms the final record decoded
	} {
		if !strings.Contains(s, want) {
			t.Errorf("tx-settings.txt missing %q", want)
		}
	}
}

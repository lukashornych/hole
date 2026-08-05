//go:build linux || darwin

package engine

import (
	"os"
	"testing"
)

// TestIsTerminal pins the distinction the attach path depends on: /dev/null is a character
// device but not a terminal, and treating it as one makes every non-interactive `hole start`
// fail with "cannot attach stdin to a TTY-enabled container".
func TestIsTerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	if info, err := devNull.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("%s is expected to be a character device; this test no longer covers the case it was written for", os.DevNull)
	}
	if IsTerminal(devNull) {
		t.Errorf("%s reported as a terminal", os.DevNull)
	}

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = readEnd.Close(); _ = writeEnd.Close() }()
	if IsTerminal(readEnd) {
		t.Error("a pipe reported as a terminal")
	}

	// A pty master is the only terminal a test can rely on having.
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	if !IsTerminal(ptmx) {
		t.Error("a pty master reported as not a terminal")
	}
}

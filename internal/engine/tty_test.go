//go:build linux || darwin

package engine

import (
	"os"
	"runtime"
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

	// Something that really is a terminal, in order of preference: the controlling terminal
	// exists whenever the suite runs from one, and a pty master stands in where it does not.
	//
	// The master is a Linux-only fallback on purpose. On Darwin the termios ioctls belong to the
	// slave side, so the master answers ENOTTY and is not a terminal by this test — which is why
	// asserting on it failed on macOS while passing on Linux.
	candidates := []string{"/dev/tty"}
	if runtime.GOOS == "linux" {
		candidates = append(candidates, "/dev/ptmx")
	}
	for _, path := range candidates {
		terminal, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		defer func() { _ = terminal.Close() }()
		if !IsTerminal(terminal) {
			t.Errorf("%s reported as not a terminal", path)
		}
		return
	}
	t.Skipf("no terminal available to check the positive case (tried %v)", candidates)
}

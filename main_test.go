package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_EndToEnd builds barfi and runs it against an in-process fake BUS
// server. Verifies that the share URL is printed to stdout and stderr is
// empty in --quiet mode.
func TestCLI_EndToEnd(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "barfi")
	cmd := exec.Command("go", "build", "-o", bin, "./")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	_, srv := newFakeBus(t)

	filePath := filepath.Join(t.TempDir(), "hello.bin")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("x"), int(MinPartSize)), 0o644); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	run := exec.Command(bin, "--server", srv.URL, "--quiet", filePath)
	run.Stdout = out
	run.Stderr = errOut
	// Isolate config: point HOME + XDG_CONFIG_HOME at a tempdir so the test
	// can't touch the developer's real ~/.config/barfi/config.json.
	run.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	if err := run.Run(); err != nil {
		t.Fatalf("barfi exited %v\nstderr: %s", err, errOut.String())
	}
	line := strings.TrimSpace(out.String())
	if !strings.HasSuffix(line, "/final-id") {
		t.Errorf("stdout = %q, want suffix /final-id", line)
	}
	if errOut.Len() != 0 {
		t.Errorf("--quiet: stderr should be empty, got: %s", errOut.String())
	}
}

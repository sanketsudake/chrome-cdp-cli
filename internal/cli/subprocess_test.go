package cli_test

// Thin subprocess layer of the command-boundary seam: build and exec the real
// binary to prove actual process exit codes (the in-process tests cover the
// envelope shape).

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	name := "chrome-cdp"
	if runtime.GOOS == "windows" {
		// A `go build -o` target with no extension on windows produces a file
		// exec can't launch (exec.Command's own lookup expects .exe), which
		// surfaces as ProcessState.ExitCode() == -1 rather than a build error.
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	out, err := exec.Command("go", "build", "-o", bin, "github.com/sanketsudake/chrome-cdp-cli/cmd/chrome-cdp").CombinedOutput()
	if err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	return bin
}

func TestBinaryExitCodes(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"exit-codes ok", []string{"exit-codes"}, 0},
		{"version ok", []string{"version"}, 0},
		{"missing required arg is usage", []string{"raw"}, 2},
		{"unknown command is usage", []string{"frobnicate"}, 2},
		{"unknown flag is usage", []string{"list", "--nope"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(bin, c.args...)
			_ = cmd.Run()
			if got := cmd.ProcessState.ExitCode(); got != c.want {
				t.Errorf("exit(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}

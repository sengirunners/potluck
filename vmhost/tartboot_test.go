package vmhost

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A box boots with --no-usb-accessories when the host's Tart lists it in
// `run --help`, and without it (still booting) on a Tart too old to know it.
func TestTartBootDropsUSBAccessoriesWhenTartKnowsTheFlag(t *testing.T) {
	for name, c := range map[string]struct {
		help string
		want bool
	}{
		"Tart 2.38.0 or newer": {"  --no-clipboard  Disable clipboard.\n  --no-usb-accessories    Disable USB accessories.\n", true},
		"older Tart":           {"  --no-clipboard  Disable clipboard.\n", false},
		"help unreadable":      {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args")
			bin := filepath.Join(dir, "tart")
			help := "printf '%s' " + strconv.Quote(c.help)
			if c.help == "" {
				help = "exit 3"
			}
			script := "#!/bin/sh\nif [ \"$1 $2\" = 'run --help' ]; then " + help + "; exit 0; fi\n[ \"$1\" = run ] && printf '%s\\n' \"$@\" > " + strconv.Quote(argsFile) + "\nexit 0\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, isolated := range []bool{true, false} {
				cmd, held, err := tartCLI{bin: bin}.Boot("zmrt-x-r1", isolated, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				held.Close()
				b, err := os.ReadFile(argsFile)
				if err != nil {
					t.Fatal(err)
				}
				args := strings.Split(strings.TrimSpace(string(b)), "\n")
				if got := slices.Contains(args, "--no-usb-accessories"); got != c.want {
					t.Errorf("isolated=%v: tart %q: --no-usb-accessories %v, want %v", isolated, args, got, c.want)
				}
				if !slices.Contains(args, "--no-clipboard") || args[len(args)-1] != "zmrt-x-r1" {
					t.Errorf("isolated=%v: tart %q lost the rest of the boot policy", isolated, args)
				}
			}
		})
	}
}

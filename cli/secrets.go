package cli

// `--secret` for sandbox exec (copied from zm's cmd/zm secrets_flag.go): the
// member's own values, read here and sent in the request body, never put in
// argv or the environment of anything potluck starts.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// secretFlagName is SPEC 20's secret name rule, checked before a name is
// used or shown: a mistyped value given as a name is refused unseen.
var secretFlagName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// secretsCap matches the daemon's 64 KiB of names and values.
const secretsCap = 64 << 10

// readSecrets turns --secret flags into the request's secrets: NAME=@path
// reads a regular file's bytes exactly (a trailing newline included), NAME
// alone reads $NAME. Errors never show a value, nor a name that fails the
// rule (it may be a value typed in the wrong place).
func readSecrets(flags []string, getenv func(string) string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	total := 0
	for i, f := range flags {
		name, src, hasSrc := strings.Cut(f, "=")
		if !secretFlagName.MatchString(name) {
			return nil, fmt.Errorf("--secret #%d: the name must be [A-Z][A-Z0-9_]{0,63} (not shown: it may be a value)", i+1)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--secret %s given twice", name)
		}
		var v string
		switch {
		case !hasSrc:
			v = getenv(name)
		case strings.HasPrefix(src, "@"):
			b, err := readBoxSecretFile(src[1:])
			if err != nil {
				return nil, fmt.Errorf("--secret %s: %v", name, err)
			}
			v = string(b)
		default:
			return nil, fmt.Errorf("--secret %s=...: give NAME=@file or NAME (from $NAME); a value on the command line would sit in your shell history and ps", name)
		}
		switch {
		case v == "" && !hasSrc: // the name may be a value typed without NAME=: not shown
			return nil, fmt.Errorf("--secret #%d: the environment variable it names is unset or empty", i+1)
		case v == "":
			return nil, fmt.Errorf("--secret %s: the file is empty", name)
		}
		if total += len(name) + len(v); total > secretsCap {
			return nil, fmt.Errorf("--secret: over 64 KiB of names and values")
		}
		out[name] = v
	}
	return out, nil
}

// readBoxSecretFile reads a regular file (a symlink to one is fine) of at most
// secretsCap bytes: never a FIFO, a device or a directory.
func readBoxSecretFile(path string) ([]byte, error) {
	// Stat before open: opening a FIFO blocks until something writes to it.
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() { // swapped between the stat and the open
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, secretsCap+1))
	if err != nil {
		return nil, err
	}
	if len(b) > secretsCap {
		return nil, fmt.Errorf("%s is over 64 KiB", path)
	}
	return b, nil
}

// envOf is where --secret NAME reads $NAME.
var envOf = os.Getenv

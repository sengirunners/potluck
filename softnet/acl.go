package softnet

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// aclEntries lists path's ACL entries as macOS's `ls -lde` prints them
// ("group:staff allow write,append"), or none on a system without them.
// A test replaces it.
var aclEntries = func(path string) ([]string, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	out, err := exec.Command("/bin/ls", "-lde", path).Output()
	if err != nil {
		return nil, fmt.Errorf("cannot read the ACL of %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var es []string
	for _, l := range lines[1:] {
		// " 0: group:everyone deny delete"
		_, e, ok := strings.Cut(l, ": ")
		if !ok {
			return nil, fmt.Errorf("cannot read the ACL of %s: %q", path, l)
		}
		es = append(es, strings.TrimSpace(e))
	}
	return es, nil
}

// aclGrants are the ACL rights that let someone other than the owner
// change a file or what a directory holds.
var aclGrants = map[string]bool{
	"write": true, "append": true, "writeattr": true, "writeextattr": true, "writesecurity": true,
	"chown": true, "delete": true, "add_file": true, "add_subdirectory": true, "delete_child": true,
}

// aclSafe refuses a path whose ACL lets anyone write, delete, add to it
// or change its owner or permissions. Deny entries are harmless; an
// entry it cannot read counts as unsafe.
func aclSafe(path string) error {
	es, err := aclEntries(path)
	if err != nil {
		return err
	}
	for _, e := range es {
		f := strings.Fields(e)
		i := -1
		for j, w := range f {
			if w == "allow" || w == "deny" {
				i = j
				break
			}
		}
		if i < 1 || i != len(f)-2 {
			return fmt.Errorf("%s has an ACL entry Potluck cannot read (%q), so it is not trusted", path, e)
		}
		if f[i] == "deny" {
			continue
		}
		for _, r := range strings.Split(f[i+1], ",") {
			if aclGrants[r] {
				return fmt.Errorf("%s has an ACL entry that lets %s change it (%q): remove it with `sudo chmod -N %q`", path, f[0], e, path)
			}
		}
	}
	return nil
}

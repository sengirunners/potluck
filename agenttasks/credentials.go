package agenttasks

import (
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/protocol"
)

func (a Adapter) checkCredentialFD() error {
	for _, name := range a.Secrets {
		if !secretName.MatchString(a.CredentialFD[name]) || a.Accepts(a.CredentialFD[name]) {
			return fmt.Errorf("adapter %s: no valid credential descriptor setting for %s", a.Name, name)
		}
	}
	for name := range a.Env {
		for _, fd := range a.CredentialFD {
			if name == fd {
				return fmt.Errorf("adapter %s: credential descriptor in environment settings", a.Name)
			}
		}
		for _, forbidden := range a.UnsetEnv {
			if name == forbidden {
				return fmt.Errorf("adapter %s: conflicting credential environment setting", a.Name)
			}
		}
	}
	for _, name := range a.UnsetEnv {
		if !secretName.MatchString(name) {
			return fmt.Errorf("adapter %s: invalid environment setting to unset", a.Name)
		}
	}
	return nil
}

// CredentialCommand reads an existing door file through descriptor 3. No token
// enters argv or the environment, including the environment inherited by tools.
// Claude 2.1.319 caches its initial descriptor read; this does not rotate a login
// in a running session. Remote mode must stay off: it persists descriptor auth.
func (a Adapter) CredentialCommand(name, model, effort string) ([]string, error) {
	argv, err := a.Command(model, effort)
	if err != nil {
		return nil, err
	}
	if !a.Accepts(name) {
		return nil, fmt.Errorf("adapter %s: unsupported credential name", a.Name)
	}
	unset := append([]string(nil), a.UnsetEnv...)
	for _, n := range a.Secrets {
		unset = append(unset, n, a.CredentialFD[n])
	}
	return append([]string{"sh", "-c", credentialEntry, "potluck-agent-credential", name, a.CredentialFD[name], strings.Join(unset, " ")}, argv...), nil
}

const credentialEntry = `name=$1 fd=$2 clean=$3; shift 3
s=` + protocol.ShSecretsDir + `
if [ -z "$s" ] || [ ! -f "$s/$name" ] || [ ! -r "$s/$name" ]; then echo "potluck: the agent credential did not arrive in the box" >&2; exit 125; fi
for n in $clean; do unset "$n"; done
exec 3< "$s/$name" || exit 125
export "$fd=3"
exec "$@"
`

package agenttasks

import "strings"

// ProxyCommand runs a task's cooperative proxy before the credential launcher.
// The helper inherits neither raw agent credentials nor their descriptors. The
// credential command opens its own descriptor only when the proxy starts it.
// Entry's timeout and process group own both the helper and its descendants.
func (a Adapter) ProxyCommand(dir string, argv []string) ([]string, error) {
	if err := a.Check(); err != nil {
		return nil, err
	}
	clean := append([]string(nil), a.UnsetEnv...)
	for _, name := range a.Secrets {
		clean = append(clean, name, a.CredentialFD[name])
	}
	return append([]string{"sh", "-c", proxyEntry, "potluck-task-proxy", dir, strings.Join(clean, " ")}, argv...), nil
}

const proxyEntry = `case "$1" in /*) d=$1 ;; *) d=$HOME/$1 ;; esac
clean=$2; shift 2
for name in $clean; do unset "$name"; done
exec 3<&-
exec python3 "$d/proxy.py" "$d/proxy.json" "$@"
`

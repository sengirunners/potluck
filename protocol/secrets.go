package protocol

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
)

// Secrets (docs/protocol.md, Secrets and Box secrets). A secret is a name and
// a value: exec's and spawn's secrets live for one command, the secrets
// verb's for the life of the box. Both land the same way: one 0600 file per
// name in a 0700 directory on a memory filesystem in the guest, its path in
// POTLUCK_SECRETS (and ZM_SECRETS), never a value in an environment, an
// argument, a VM's config, a record, a log or a disk.
const (
	// CapSecrets: exec and spawn take secrets.
	CapSecrets = "secrets"
	// CapBoxSecrets: the driver serves the secrets verb.
	CapBoxSecrets = "box-secrets"
	// VerbSecrets sets and unsets a box's secrets.
	VerbSecrets = "secrets"
	// SecretsMax bounds names and values together: one exec's secrets, and
	// everything a box holds through the secrets verb.
	SecretsMax = 64 << 10
	// SecretsUnsetMax bounds one secrets call's unset list.
	SecretsUnsetMax = 256
	// BoxSecretsDir is where a guest with a memory filesystem at /dev/shm
	// keeps its box's secrets: a fixed path for the box's life.
	BoxSecretsDir = "/dev/shm/potluck-secrets"
	// ShSecretsDir is a guest shell's expansion of the command's secrets
	// directory: POTLUCK_SECRETS, or ZM_SECRETS from an older driver.
	ShSecretsDir = "${POTLUCK_SECRETS:-$ZM_SECRETS}"
)

var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// SecretName reports whether n is a secret's name: [A-Z][A-Z0-9_]{0,63}.
func SecretName(n string) bool { return secretName.MatchString(n) }

// BoxSecretsArgs are the secrets verb's args: secrets to set (each replaces
// the file of that name whole) and names to unset. The values travel under
// "secrets", the key the daemon seals on every leg that needs it.
type BoxSecretsArgs struct {
	Secrets map[string]string `json:"secrets,omitempty"`
	Unset   []string          `json:"unset,omitempty"`
}

// BoxSecretsResult is the secrets verb's answer: the directory (a path,
// never a value) and how many names the box holds now.
type BoxSecretsResult struct {
	Dir   string `json:"dir"`
	Count int    `json:"count"`
}

// Refusals of a secrets call. None names a name or a value.
var (
	ErrSecretsShape = errors.New("secrets is an object of names to string values, unset an array of names")
	ErrSecretsEmpty = errors.New("a secrets call sets or unsets at least one name")
	ErrSecretName   = errors.New("a secret's name is not [A-Z][A-Z0-9_]{0,63}")
	ErrSecretsBig   = errors.New("secrets are over 64 KiB (names and values)")
	ErrSecretsBoth  = errors.New("a name is both set and unset in one call")
	ErrUnsetMany    = errors.New("unset names at most 256 names")
)

// ParseBoxSecrets reads and checks a secrets call's args. Its refusals quote
// nothing it was sent (Go's decode errors would name a key's path).
func ParseBoxSecrets(raw json.RawMessage) (BoxSecretsArgs, error) {
	var a struct {
		Secrets json.RawMessage `json:"secrets"`
		Unset   json.RawMessage `json:"unset"`
	}
	var out BoxSecretsArgs
	if len(raw) > 0 && json.Unmarshal(raw, &a) != nil {
		return out, ErrSecretsShape
	}
	if set(a.Secrets) && (a.Secrets[0] != '{' || json.Unmarshal(a.Secrets, &out.Secrets) != nil) {
		return BoxSecretsArgs{}, ErrSecretsShape
	}
	if set(a.Unset) && (a.Unset[0] != '[' || json.Unmarshal(a.Unset, &out.Unset) != nil) {
		return BoxSecretsArgs{}, ErrSecretsShape
	}
	return out, out.Check()
}

func set(raw json.RawMessage) bool {
	for len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\n' || raw[0] == '\t' || raw[0] == '\r') {
		raw = raw[1:]
	}
	return len(raw) > 0 && string(raw) != "null"
}

// Check refuses what the protocol refuses in one call: nothing to do, a bad
// name, a name set and unset, more than 64 KiB set, more than 256 unset.
// Whether the box's total stays within 64 KiB is the driver's to check.
func (a BoxSecretsArgs) Check() error {
	if len(a.Secrets) == 0 && len(a.Unset) == 0 {
		return ErrSecretsEmpty
	}
	if len(a.Unset) > SecretsUnsetMax {
		return ErrUnsetMany
	}
	n := 0
	for k, v := range a.Secrets {
		if !SecretName(k) {
			return ErrSecretName
		}
		n += len(k) + len(v)
	}
	for _, k := range a.Unset {
		if !SecretName(k) {
			return ErrSecretName
		}
		if _, ok := a.Secrets[k]; ok {
			return ErrSecretsBoth
		}
	}
	if n > SecretsMax {
		return ErrSecretsBig
	}
	return nil
}

// Names are the names a call sets and unsets, sorted, each once.
func (a BoxSecretsArgs) Names() []string {
	out := slices.Clone(a.Unset)
	for k := range a.Secrets {
		out = append(out, k)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

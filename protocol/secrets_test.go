package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A secrets call's args: what they may hold, and refusals that quote nothing.
func TestParseBoxSecrets(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want error
	}{
		{`{"secrets":{"API_KEY":"v"}}`, nil},
		{`{"unset":["API_KEY"]}`, nil},
		{`{"secrets":{"A":"v"},"unset":["B"]}`, nil},
		{`{}`, ErrSecretsEmpty},
		{``, ErrSecretsEmpty},
		{`{"secrets":null,"unset":[]}`, ErrSecretsEmpty},
		{`{"secrets":"zzqvalue"}`, ErrSecretsShape},
		{`{"secrets":{"ZZQNAME":12345}}`, ErrSecretsShape},
		{`{"unset":"ZZQNAME"}`, ErrSecretsShape},
		{`[1]`, ErrSecretsShape},
		{`{"secrets":{"zzq_lower":"v"}}`, ErrSecretName},
		{`{"secrets":{"":"v"}}`, ErrSecretName},
		{`{"unset":["ZZQ BAD"]}`, ErrSecretName},
		{`{"secrets":{"ZZQ":"v"},"unset":["ZZQ"]}`, ErrSecretsBoth},
		{`{"secrets":{"BIG":"` + strings.Repeat("x", SecretsMax) + `"}}`, ErrSecretsBig},
		{`{"secrets":{"BIG":"` + strings.Repeat("x", SecretsMax-3) + `"}}`, nil},
		{`{"unset":[` + strings.TrimSuffix(strings.Repeat(`"A",`, SecretsUnsetMax+1), ",") + `]}`, ErrUnsetMany},
	} {
		a, err := ParseBoxSecrets(json.RawMessage(c.raw))
		if !errors.Is(err, c.want) {
			t.Errorf("%.60s: %v, want %v", c.raw, err, c.want)
		}
		if err != nil && (strings.Contains(err.Error(), "zzq") || strings.Contains(err.Error(), "ZZQ") || strings.Contains(err.Error(), "12345")) {
			t.Errorf("%s: the refusal quotes what was sent: %v", c.raw, err)
		}
		if err == nil && len(a.Names()) == 0 {
			t.Errorf("%.60s: no names", c.raw)
		}
	}
}

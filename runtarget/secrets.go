package runtarget

// Renter secrets on a box exec (SPEC 20 "Boxes", Secrets), split from
// runtarget.go. Owned by sandboxes (zm-eakh.12.7.4) inside run targets.

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

var secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// secretsAllowed is SPEC 20 "Boxes", Secrets: names and size, a driver
// listing the capability, a trusted host. A refusal names neither a value
// nor a name. A binding's driver on this machine is the owner's own and so
// trusted ("Host class"); one served from another machine asks
// teamlink for that host's class (hostTrusted, remote.go).
func (s *Service) secretsAllowed(ctx context.Context, b computeapi.RunTarget, secrets map[string]string) error {
	n := 0
	for k, v := range secrets {
		if !secretName.MatchString(k) {
			return api.Errorf(api.CodeBadRequest, "a secret name is not [A-Z][A-Z0-9_]{0,63}")
		}
		n += len(k) + len(v)
	}
	if n > secretsMax {
		return api.Errorf(api.CodeBadRequest, "secrets are over 64 KiB (names and values)")
	}
	if err := s.hostTrusted(ctx, b, "secrets"); err != nil { // a public host: never, whatever its driver lists
		return err
	}
	if err := s.callerSealed(ctx, "secrets"); err != nil { // sent in the clear by a machine that cannot seal
		return err
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return err
	}
	if !slices.Contains(d.Capabilities, "secrets") {
		return api.Errorf(api.CodeBadRequest, "run target %s takes no secrets (its driver lists no secrets capability)", b.Name)
	}
	return nil
}

// scrubSecrets takes every secret name and value (4 bytes or more) out of
// a driver's refusal before it reaches the caller or a log: a driver may
// quote what it was sent.
func scrubSecrets(err error, secrets map[string]string) error {
	if len(secrets) == 0 {
		return err
	}
	var e *api.Error
	if !errors.As(err, &e) {
		return api.Errorf(api.CodeInternal, "the driver's exec failed (message withheld: secrets were sent)")
	}
	msg := e.Message
	for k, v := range secrets {
		for _, x := range []string{v, k} {
			if len(x) >= 4 {
				msg = strings.ReplaceAll(msg, x, "[secret]")
			}
		}
	}
	return api.Errorf(e.Code, "%s", msg)
}

// secretsMax bounds one exec's secrets, names and values together.
const secretsMax = 64 << 10

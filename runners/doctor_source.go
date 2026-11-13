package runners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"time"
)

// PersonRead reads GitHub using the machine owner's gh sign-in. It is used
// only by the local doctor, its broker self-check and the local setup route (repository ids for the install page),
// never for a remote caller.
// A test may supply an in-memory reader instead.
type PersonRead func(context.Context, string, any) error

// doctorCallBudget is how many gh reads one doctor command or self-check
// makes at most, pagination included: GitHub allows a person 5000 an hour.
const doctorCallBudget = 60

type doctorRunKey struct{}
type doctorRun struct {
	calls   int
	refused bool                   // a read was refused for the budget
	pools   map[string]*doctorPool // by pool project: the run's one listing (doctorPool)
}

func doctorContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(doctorRunKey{}).(*doctorRun); ok {
		return ctx
	}
	return context.WithValue(ctx, doctorRunKey{}, &doctorRun{})
}
func doctorBudgetSpent(ctx context.Context) bool {
	r, ok := ctx.Value(doctorRunKey{}).(*doctorRun)
	return ok && r.calls >= doctorCallBudget
}

// doctorRefused reports whether a read of this run was refused for the
// budget (and resets it): what it was for is incomplete.
func doctorRefused(ctx context.Context) bool {
	r, ok := ctx.Value(doctorRunKey{}).(*doctorRun)
	if !ok || !r.refused {
		return false
	}
	r.refused = false
	return true
}

var errDoctorBudget = errors.New("unavailable: doctor GitHub call budget exhausted; check a named repository separately")

func (s *Service) personRead(ctx context.Context, path string, out any) error {
	if r, ok := ctx.Value(doctorRunKey{}).(*doctorRun); ok {
		if r.calls >= doctorCallBudget {
			r.refused = true
			return errDoctorBudget
		}
		r.calls++
	}

	if s.DoctorRead != nil {
		return s.DoctorRead(ctx, path, out)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u, err := url.Parse(s.conf().GitHub.WebURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("no GitHub hostname configured")
	}
	b, err := exec.CommandContext(ctx, "gh", "api", "--hostname", u.Hostname(), "--method", "GET", path).Output()
	if err != nil {
		return fmt.Errorf("unavailable: gh could not read GitHub; check connectivity, repository access and gh authentication")
	}
	return json.Unmarshal(b, out)
}

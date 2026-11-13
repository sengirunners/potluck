package runtarget

// Terminals in a box (SPEC 20 "Boxes", terminals; zm-eakh.12.5.21): spawn's
// pty and the optional input verb, the holder's like the other background
// process verbs, over the same driver calls and relay. A Ctrl-C or Ctrl-D is
// a byte the box's terminal acts on; zm passes bytes and sizes, nothing
// else. zm sends a pty only to a driver listing capability pty: an older one
// would run the command without a terminal, silently.

import (
	"context"
	"encoding/base64"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

const (
	verbInput = "input"
	ptyCap    = "pty"
	inputMax  = 64 << 10
	ptySide   = 1000
	// inputLimit is an input call's time at the driver: it waits at most
	// 10 s on a terminal that reads nothing.
	inputLimit = 15 * time.Second
)

var ptyTerm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// ptyAllowed checks spawn's pty: its size and term, no secrets, a driver
// that lists it.
func (s *Service) ptyAllowed(ctx context.Context, b computeapi.RunTarget, req computeapi.SpawnRunTargetRequest) error {
	p := req.PTY
	if p == nil {
		return nil
	}
	switch {
	case p.Rows < 1 || p.Rows > ptySide || p.Cols < 1 || p.Cols > ptySide:
		return api.Errorf(api.CodeBadRequest, "pty rows and cols are 1..%d", ptySide)
	case p.Term != "" && !ptyTerm.MatchString(p.Term):
		return api.Errorf(api.CodeBadRequest, "pty term is a terminal type name like xterm-256color")
	case len(req.Secrets) > 0:
		return api.Errorf(api.CodeBadRequest, "a process on a terminal takes no secrets (the terminal would echo them)")
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return err
	}
	if !slices.Contains(d.Capabilities, ptyCap) {
		return api.Errorf(api.CodeBadRequest, "run target %s gives no terminal (its driver lists no pty capability)", b.Name)
	}
	return nil
}

// input writes bytes to a background process's terminal, or resizes it.
func (s *Service) input(r *http.Request) (any, error) {
	b, rw, err := s.boxHolder(r)
	if err != nil {
		return nil, err
	}
	var req computeapi.InputRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if err := checkPID(req.PID); err != nil {
		return nil, err
	}
	n := len(req.Data)
	if req.Encoding == computeapi.EncodingBase64 {
		raw, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "data is not base64")
		}
		n = len(raw)
	}
	switch {
	case req.Encoding != "" && req.Encoding != computeapi.EncodingBase64:
		return nil, api.Errorf(api.CodeBadRequest, "encoding is base64 or absent")
	case n > inputMax:
		return nil, api.Errorf(api.CodeBadRequest, "data is over 64 KiB")
	case (req.Rows == 0) != (req.Cols == 0) || req.Rows < 0 || req.Rows > ptySide || req.Cols < 0 || req.Cols > ptySide:
		return nil, api.Errorf(api.CodeBadRequest, "rows and cols go together, each 1..%d", ptySide)
	}
	// No capability check for base64: every driver serving input takes it
	// (SPEC 20), and one without input answers not_implemented itself.
	var out computeapi.RunTargetInput
	return out, s.callWithin(r.Context(), b, verbInput, rw.Handle, req, &out, inputLimit)
}

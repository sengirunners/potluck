package runtarget

// Background processes in a box (SPEC 20 "Boxes", background processes):
// spawn, output and signal, optional driver verbs for the box's holder, and
// exec's base64 encoding. Each call answers well inside the 30 s a member's
// proxy and Cloudflare's tunnel allow, so a long command is started, read in
// pages and stopped from any machine. A driver without the verbs answers
// not_implemented itself (SPEC 11), so zm asks describe only for what an
// older driver would get wrong silently: base64, and secrets. Owned by run
// targets (zm-eakh.12.5.15).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

const (
	verbSpawn  = "spawn"
	verbOutput = "output"
	verbSignal = "signal"

	outputMax     = 1 << 20
	outputDefault = 64 << 10
	outputWaitMax = 20
	// relayOutputMax bounds one relayed output page: a page of bytes the
	// encoder escapes to 6 each stays under the host's 3 MiB answer.
	relayOutputMax = 256 << 10
)

// outputLimit is how long an output call may take at the driver: its
// wait_s plus 10 s, never past the 30 s verb limit.
func outputLimit(waitS int) time.Duration {
	return min(time.Duration(min(max(waitS, 0), outputWaitMax))*time.Second+10*time.Second, verbLimit)
}

// checkCommand is exec's and spawn's command check.
func checkCommand(argv []string, stdin, cwd, encoding string) error {
	n := len(stdin)
	if encoding == computeapi.EncodingBase64 {
		n = base64.StdEncoding.DecodedLen(len(stdin))
	}
	switch {
	case len(argv) == 0:
		return api.Errorf(api.CodeBadRequest, "argv must be a non-empty array")
	case len(argv) > execMaxArgv:
		return api.Errorf(api.CodeBadRequest, "argv has over %d words", execMaxArgv)
	case encoding != "" && encoding != computeapi.EncodingBase64:
		return api.Errorf(api.CodeBadRequest, "encoding is base64 or absent")
	case n > execMaxStdin:
		return api.Errorf(api.CodeBadRequest, "stdin is over 1 MiB")
	case len(cwd) > 4096 || strings.ContainsRune(cwd, 0):
		return api.Errorf(api.CodeBadRequest, "cwd is over 4096 bytes or holds a NUL")
	}
	for _, a := range argv {
		if len(a) > 64<<10 || strings.ContainsRune(a, 0) {
			return api.Errorf(api.CodeBadRequest, "an argv word is over 64 KiB or holds a NUL")
		}
	}
	return nil
}

// encodingAllowed refuses base64 to a driver that does not list it: an
// older one would take the base64 text as the bytes.
func (s *Service) encodingAllowed(ctx context.Context, b computeapi.RunTarget, encoding string) error {
	if encoding == "" {
		return nil
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return err
	}
	if !slices.Contains(d.Capabilities, "base64") {
		return api.Errorf(api.CodeBadRequest, "run target %s takes no encoding base64 (its driver lists no base64 capability)", b.Name)
	}
	return nil
}

// capExec holds exec's 1 MiB cap on what a driver answers, in bytes as
// sent (base64 decoded).
func capExec(out *computeapi.RunTargetExec) {
	for _, o := range []*string{&out.Stdout, &out.Stderr} {
		switch {
		case out.Encoding == computeapi.EncodingBase64 && len(*o) > base64.StdEncoding.EncodedLen(execMaxOut):
			*o, out.Truncated = (*o)[:base64.StdEncoding.EncodedLen(execMaxOut)], true
		case out.Encoding != computeapi.EncodingBase64 && len(*o) > execMaxOut:
			*o, out.Truncated = strings.ToValidUTF8((*o)[:execMaxOut], "�"), true
		}
	}
}

// sandboxID is how errors name a box: its handle without "box:", as the
// potluck CLI prints a sandbox's id.
func sandboxID(handle string) string { return strings.TrimPrefix(handle, "box:") }

// boxHolder is the path's box for its holder, ready and before expires_at.
func (s *Service) boxHolder(r *http.Request) (computeapi.RunTarget, row, error) {
	_, b, rw, err := s.boxHolderIn(r)
	return b, rw, err
}

// boxHolderIn is boxHolder with the project's ULID.
func (s *Service) boxHolderIn(r *http.Request) (string, computeapi.RunTarget, row, error) {
	pid, b, _, rw, err := s.loadHandle(r, authz.Operate)
	if err != nil {
		return pid, b, rw, err
	}
	if b.Kind != computeapi.RunTargetKindBox {
		return pid, b, rw, api.Errorf(api.CodeBadRequest, "%s is a %s target; background processes are a box's", b.Name, b.Kind)
	}
	if err := holder(r.Context(), rw, false); err != nil {
		return pid, b, rw, err
	}
	if rw.State != stateReady {
		return pid, b, rw, api.Errorf(api.CodeConflict, "sandbox %s is %s, not ready", sandboxID(rw.Handle), rw.State)
	}
	if offPin(b, rw) {
		return pid, b, rw, api.Errorf(api.CodeConflict, "sandbox %s booted %s, not one of the pinned images; it is being released", sandboxID(rw.Handle), rw.ImageDigest)
	}
	if exp, err := time.Parse(time.RFC3339Nano, rw.ExpiresAt); err == nil && !time.Now().Before(exp) {
		return pid, b, rw, api.Errorf(api.CodeConflict, "sandbox %s is past its expires_at", sandboxID(rw.Handle))
	}
	return pid, b, rw, nil
}

func checkPID(pid string) error {
	if pid == "" || len(pid) > 64 || strings.ContainsFunc(pid, func(r rune) bool { return r <= ' ' || r == '/' || r > '~' }) {
		return api.Errorf(api.CodeBadRequest, "bad pid %q", pid)
	}
	return nil
}

// spawn starts a command in the background in a box, for its holder.
func (s *Service) spawn(r *http.Request) (any, error) {
	pid, b, rw, err := s.boxHolderIn(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var body struct { // secrets decode on their own, as exec's
		computeapi.SpawnRunTargetRequest
		Secrets json.RawMessage `json:"secrets"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	req := body.SpawnRunTargetRequest
	if t := strings.TrimSpace(string(body.Secrets)); t != "" && t != "null" {
		if err := json.Unmarshal(body.Secrets, &req.Secrets); err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "secrets must be an object of string values")
		}
	}
	if err := checkCommand(req.Argv, req.Stdin, req.Cwd, req.Encoding); err != nil {
		return nil, err
	}
	switch {
	case req.RequestID == "" || len(req.RequestID) > 200 || strings.ContainsFunc(req.RequestID, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return nil, api.Errorf(api.CodeBadRequest, "spawn needs a request_id (at most 200 bytes, no control characters)")
	case req.TimeoutS < 0:
		return nil, api.Errorf(api.CodeBadRequest, "timeout_s must not be negative (0: until the box ends)")
	}
	if err := s.encodingAllowed(ctx, b, req.Encoding); err != nil {
		return nil, err
	}
	if err := s.ptyAllowed(ctx, b, req); err != nil { // pty.go
		return nil, err
	}
	args := map[string]any{"argv": req.Argv, "request_id": req.RequestID}
	if req.PTY != nil {
		args["pty"] = req.PTY
	}
	for k, v := range map[string]string{"stdin": req.Stdin, "cwd": req.Cwd, "encoding": req.Encoding} {
		if v != "" {
			args[k] = v
		}
	}
	if req.TimeoutS > 0 {
		args["timeout_s"] = req.TimeoutS
	}
	dec, err := s.secretsGranted(ctx, pid, b, rw, verbSpawn, req.Secrets) // grants.go: before anything is sent
	if err != nil {
		return nil, err
	}
	if len(req.Secrets) > 0 {
		if err := s.secretsAllowed(ctx, b, req.Secrets); err != nil {
			return nil, err
		}
		args["secrets"] = req.Secrets
	}
	var out computeapi.RunTargetSpawn
	if err := s.secretsSent(ctx, dec, func() error { return s.call(ctx, b, verbSpawn, rw.Handle, args, &out) }); err != nil {
		return nil, scrubSecrets(err, req.Secrets)
	}
	if checkPID(out.PID) != nil {
		return nil, api.Errorf(api.CodeInternal, "potluck-driver-%s spawn answered no usable pid", b.Driver)
	}
	return out, nil
}

// output pages a background process's stdout and stderr, for the holder.
func (s *Service) output(r *http.Request) (any, error) {
	b, rw, err := s.boxHolder(r)
	if err != nil {
		return nil, err
	}
	var req computeapi.OutputRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if err := checkPID(req.PID); err != nil {
		return nil, err
	}
	switch {
	case req.StdoutFrom < 0 || req.StderrFrom < 0:
		return nil, api.Errorf(api.CodeBadRequest, "stdout_from and stderr_from are byte offsets, 0 or more")
	case req.MaxBytes < 0 || req.MaxBytes > outputMax:
		return nil, api.Errorf(api.CodeBadRequest, "max_bytes is at most %d", outputMax)
	case req.WaitS < 0 || req.WaitS > outputWaitMax:
		return nil, api.Errorf(api.CodeBadRequest, "wait_s is 0..%d", outputWaitMax)
	case req.Encoding != "" && req.Encoding != computeapi.EncodingBase64:
		return nil, api.Errorf(api.CodeBadRequest, "encoding is base64 or absent")
	}
	if err := s.encodingAllowed(r.Context(), b, req.Encoding); err != nil {
		return nil, err
	}
	var out computeapi.RunTargetOutput
	if err := s.callWithin(r.Context(), b, verbOutput, rw.Handle, req, &out, outputLimit(req.WaitS)); err != nil {
		return nil, err
	}
	// zm holds the page bound itself: a host's output is data.
	limit := 4 * outputDefault
	if req.MaxBytes > 0 {
		limit = 4 * req.MaxBytes
	}
	for _, o := range []*string{&out.Stdout, &out.Stderr} {
		if len(*o) > limit {
			*o = strings.ToValidUTF8((*o)[:limit], "")
		}
	}
	if out.Running {
		out.Exit = nil
	}
	return out, nil
}

// signal sends TERM, INT or KILL to a background process's group.
func (s *Service) signal(r *http.Request) (any, error) {
	b, rw, err := s.boxHolder(r)
	if err != nil {
		return nil, err
	}
	var req computeapi.SignalRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if err := checkPID(req.PID); err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"TERM", "INT", "KILL"}, req.Signal) {
		return nil, api.Errorf(api.CodeBadRequest, "signal is TERM, INT or KILL")
	}
	var out computeapi.RunTargetSignal
	return out, s.call(r.Context(), b, verbSignal, rw.Handle, req, &out)
}

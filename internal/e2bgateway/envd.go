package e2bgateway

// The envd routes the SDKs call for commands.run, files.read and
// files.write, answered by the gateway with box exec (no envd in the box).
// Requests are routed by the E2b-Sandbox-Id header and authenticated by
// X-Access-Token, so one listener serves every sandbox.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/computeclient"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
)

const (
	// execCapS and execCapBytes are box exec's limits (SPEC 20 "Boxes").
	execCapS     = 600
	execCapBytes = 1 << 20
	// fileChunk is the raw bytes one exec moves: base64 of it stays well
	// under exec's 1 MiB stdin and output caps.
	fileChunk = 384 << 10
	// keepalive is how often a command stream says it is alive.
	keepalive = 50 * time.Second
)

// fileMax bounds one file read, and one request's (decompressed) upload,
// through the gateway (a var for tests).
var fileMax = 100 << 20

// errTooLarge is an upload past fileMax.
var errTooLarge = errors.New("upload too large")

// capReader fails with errTooLarge once more than n bytes were read.
type capReader struct {
	r io.Reader
	n int
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n < 0 {
		return 0, errTooLarge
	}
	if len(p) > c.n+1 {
		p = p[:c.n+1]
	}
	k, err := c.r.Read(p)
	c.n -= k
	if c.n < 0 {
		return 0, errTooLarge
	}
	return k, err
}

func (g *gateway) serveEnvd(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("E2b-Sandbox-Id")
	if host := strings.SplitN(r.Host, ".", 2)[0]; strings.HasPrefix(host, envdPort+"-") {
		if hid := strings.TrimPrefix(host, envdPort+"-"); id == "" {
			id = hid
		} else if hid != id {
			writeErr(w, http.StatusBadRequest, "E2b-Sandbox-Id does not match the host name")
			return
		}
	}
	if p := r.Header.Get("E2b-Sandbox-Port"); p != "" && p != envdPort {
		writeErr(w, http.StatusBadGateway, "potluck-e2b v1 serves envd (port %s) only, not port %s", envdPort, p)
		return
	}
	k := keyForToken(g.secret, g.keys, id, r.Header.Get("X-Access-Token"))
	if k == nil {
		g.envdErr(w, r, http.StatusUnauthorized, "unauthenticated", "invalid access token")
		return
	}
	s, _, err := g.lookup(r.Context(), k, id)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == api.CodeNotFound {
			// What E2B's edge answers for a sandbox that is gone.
			writeErr(w, http.StatusBadGateway, "The sandbox %s was not found", id)
			return
		}
		g.envdErr(w, r, http.StatusServiceUnavailable, "unavailable", "potluck: "+err.Error())
		return
	}
	switch {
	case r.URL.Path == "/health":
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/files" && r.Method == http.MethodGet:
		g.readFile(w, r, s)
	case r.URL.Path == "/files" && r.Method == http.MethodPost:
		g.writeFiles(w, r, s)
	case r.URL.Path == "/process.Process/Start" && r.Method == http.MethodPost:
		g.start(w, r, s)
	default:
		g.envdErr(w, r, http.StatusNotImplemented, "unimplemented", "potluck-e2b v1 does not serve "+r.URL.Path+" (commands.run, files.read and files.write only)")
	}
}

// envdErr answers a Connect RPC with a Connect error, and an envd HTTP
// route with envd's {"code","message"}.
func (g *gateway) envdErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if strings.Contains(r.URL.Path, ".") && strings.HasPrefix(r.Header.Get("Content-Type"), "application/") &&
		strings.Contains(r.Header.Get("Content-Type"), "json") {
		writeJSON(w, status, map[string]string{"code": code, "message": msg})
		return
	}
	writeErr(w, status, "%s", msg)
}

// boxExec runs argv in the sandbox's box.
func (g *gateway) boxExec(r *http.Request, s *sandbox, argv []string, stdin string, timeoutS int, cwd string) (computeapi.RunTargetExec, error) {
	return computeclient.ExecRunTarget(r.Context(), s.key.c, s.target, s.hid, computeapi.ExecRunTargetRequest{Argv: argv, Stdin: stdin, TimeoutS: timeoutS, Cwd: cwd})
}

// boxRun runs a command in the sandbox's box with its envs as exec
// secrets: the daemon's secrets rules apply (a trusted host, a driver
// listing secrets, the grants), and the values never enter argv.
func (g *gateway) boxRun(r *http.Request, s *sandbox, c command, timeoutS int, cwd string) (computeapi.RunTargetExec, error) {
	return computeclient.ExecRunTarget(r.Context(), s.key.c, s.target, s.hid, computeapi.ExecRunTargetRequest{Argv: c.argv, Secrets: c.secrets, TimeoutS: timeoutS, Cwd: cwd})
}

// --- commands.run: process.Process/Start, a Connect server stream ---

type startRequest struct {
	Process struct {
		Cmd  string            `json:"cmd"`
		Args []string          `json:"args"`
		Envs map[string]string `json:"envs"`
		Cwd  *string           `json:"cwd"`
	} `json:"process"`
	Pty   json.RawMessage `json:"pty"`
	Stdin bool            `json:"stdin"`
	Tag   *string         `json:"tag"`
}

// connectError is a Connect end-of-stream error.
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (g *gateway) start(w http.ResponseWriter, r *http.Request, s *sandbox) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/connect+json") {
		g.envdErr(w, r, http.StatusUnsupportedMediaType, "unimplemented", "potluck-e2b speaks the Connect protocol with JSON (application/connect+json), not "+ct)
		return
	}
	var req startRequest
	if err := readEnvelope(r.Body, &req); err != nil {
		g.envdErr(w, r, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	c, err := commandOf(req, s.envs)
	if err != nil {
		g.envdErr(w, r, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	// The client's deadline (Connect-Timeout-Ms) bounds the command; none,
	// or one past exec's cap, runs to the cap.
	timeoutS, overCap := execTimeout(r.Header.Get("Connect-Timeout-Ms"))
	cwd := ""
	if req.Process.Cwd != nil {
		cwd = *req.Process.Cwd
	}
	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	send := func(flags byte, v any) {
		b, _ := json.Marshal(v)
		var hdr [5]byte
		hdr[0] = flags
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))
		_, _ = w.Write(hdr[:])
		_, _ = w.Write(b)
		_ = rc.Flush()
	}
	event := func(e map[string]any) { send(0, map[string]any{"event": e}) }
	end := func(e *connectError) {
		if e == nil {
			send(2, map[string]any{})
			return
		}
		send(2, map[string]any{"error": e})
	}
	event(map[string]any{"start": map[string]any{"pid": 1}})
	type result struct {
		x   computeapi.RunTargetExec
		err error
	}
	done := make(chan result, 1)
	began := g.now()
	go func() {
		x, err := g.boxRun(r, s, c, timeoutS, cwd)
		done <- result{x, err}
	}()
	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	var res result
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-tick.C:
			event(map[string]any{"keepalive": map[string]any{}})
		case <-r.Context().Done():
			return
		}
	}
	if res.err != nil {
		end(&connectError{execErrCode(res.err), "potluck sandbox exec: " + res.err.Error()})
		return
	}
	x := res.x
	if x.Exit == 124 && g.now().Sub(began) >= time.Duration(timeoutS)*time.Second-time.Second {
		msg := fmt.Sprintf("the command ran past its %d s timeout and was killed", timeoutS)
		if overCap {
			msg = fmt.Sprintf("the command ran past potluck sandbox exec's %d s cap and was killed", execCapS)
		}
		end(&connectError{"deadline_exceeded", msg})
		return
	}
	if x.Truncated {
		end(&connectError{"out_of_range", fmt.Sprintf("the command's output passed potluck sandbox exec's %d byte cap (stdout or stderr); none of it is returned rather than a cut copy", execCapBytes)})
		return
	}
	if x.Stdout != "" {
		event(map[string]any{"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString([]byte(x.Stdout))}})
	}
	if x.Stderr != "" {
		event(map[string]any{"data": map[string]any{"stderr": base64.StdEncoding.EncodeToString([]byte(x.Stderr))}})
	}
	event(map[string]any{"end": map[string]any{"exitCode": x.Exit, "exited": true, "status": fmt.Sprintf("exit status %d", x.Exit)}})
	end(nil)
}

// execErrCode is the Connect code for a failed exec: a refused request is
// invalid_argument, a refused secret (envs on a public host, or no grant)
// permission_denied, anything else unavailable.
func execErrCode(err error) string {
	var ae *api.Error
	switch {
	case errors.As(err, &ae) && ae.Code == api.CodeBadRequest:
		return "invalid_argument"
	case errors.As(err, &ae) && ae.Code == api.CodeForbidden:
		return "permission_denied"
	}
	return "unavailable"
}

// execTimeout is exec's timeout_s for a Connect-Timeout-Ms header, and
// whether the client asked for more than exec allows (or no limit).
func execTimeout(hdr string) (int, bool) {
	if ms, err := strconv.ParseInt(hdr, 10, 64); err == nil && ms > 0 && ms <= execCapS*1000 {
		return int((ms + 999) / 1000), false
	}
	return execCapS, true
}

// command is what exec runs for commands.run: a literal argv, and the
// envs as secrets.
type command struct {
	argv    []string
	secrets map[string]string
}

// envWrap sets each file of the command's secrets directory as an env var
// and runs "$@". Values go from a file in guest memory to the command's
// environment, never through argv (the guest's process list shows argv).
// cat is found once, before an env named PATH can hide it; $(...) drops
// trailing newlines, and the x keeps them. A name the shell will not set
// (bash's readonly UID, PPID...) fails the command rather than vanish.
const envWrap = `s=` + protocol.ShSecretsDir + `
[ -n "$s" ] && [ -d "$s" ] || { echo "potluck-e2b: the command's envs did not arrive in the box" >&2; exit 125; }
c=$(command -v cat) || { echo "potluck-e2b: the box has no cat, which envs need" >&2; exit 125; }
for f in "$s"/*; do
	[ -f "$f" ] || continue
	n=${f##*/}; v=$("$c" "$f" && echo x) || { echo "potluck-e2b: cannot read env $n in the box" >&2; exit 125; }; v=${v%x}
	(export "$n=$v") 2>/dev/null || { echo "potluck-e2b: the box's shell will not set env $n" >&2; exit 125; }
	export "$n=$v"
done
unset s c f n v
exec "$@"`

// commandOf is exec's request for commands.run. The sandbox's envs come
// first, the command's own override them; with any, they travel as exec
// secrets and envWrap sets them in the guest. An env name is a secret's
// name ([A-Z][A-Z0-9_]{0,63}), the file it lands in.
func commandOf(req startRequest, envs map[string]string) (command, error) {
	if !empty(req.Pty) {
		return command{}, errors.New("potluck-e2b v1 has no PTY")
	}
	if req.Stdin {
		return command{}, errors.New("potluck-e2b v1 cannot stream stdin to a command (stdin=True)")
	}
	if c := req.Process.Cmd; c == "" || strings.HasPrefix(c, "-") {
		return command{}, errors.New("process.cmd must be a program (not empty, no leading '-')")
	}
	for k := range req.Process.Envs {
		if !protocol.SecretName(k) {
			return command{}, fmt.Errorf("env name %q is not [A-Z][A-Z0-9_]{0,63} (potluck-e2b sends envs as secrets, one file per name)", k)
		}
	}
	argv := append([]string{req.Process.Cmd}, req.Process.Args...)
	if len(envs) == 0 && len(req.Process.Envs) == 0 {
		return command{argv: argv}, nil
	}
	secrets := maps.Clone(envs)
	if secrets == nil {
		secrets = map[string]string{}
	}
	maps.Copy(secrets, req.Process.Envs)
	for k, v := range secrets {
		if strings.ContainsRune(v, 0) { // the guest shell's $(...) would drop it
			return command{}, fmt.Errorf("env %s holds a NUL byte, which an environment variable cannot carry", k)
		}
	}
	return command{argv: append([]string{"sh", "-c", envWrap, "sh"}, argv...), secrets: secrets}, nil
}

// readEnvelope reads one Connect envelope (flags, big-endian length, JSON).
func readEnvelope(body io.Reader, v any) error {
	var hdr [5]byte
	if _, err := io.ReadFull(body, hdr[:]); err != nil {
		return fmt.Errorf("bad Connect envelope: %v", err)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if hdr[0]&1 != 0 {
		return errors.New("compressed Connect messages are not accepted")
	}
	if n > 1<<20 {
		return errors.New("Connect message too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(body, b); err != nil {
		return fmt.Errorf("bad Connect envelope: %v", err)
	}
	return json.Unmarshal(b, v)
}

// --- files.read: GET /files?path= ---

// readScript prints the file's size, then base64 of the chunk asked for
// ($2: the chunk index, $3: its size). Exit 3: no such file, 4: a
// directory, 5: unreadable.
const readScript = `p=$1
[ -e "$p" ] || exit 3
[ -d "$p" ] && exit 4
[ -r "$p" ] || exit 5
wc -c < "$p" | tr -d ' '
dd if="$p" bs="$3" skip="$2" count=1 2>/dev/null | base64`

func (g *gateway) readFile(w http.ResponseWriter, r *http.Request, s *sandbox) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	var out []byte
	size := -1
	for i := 0; size < 0 || len(out) < size; i++ {
		x, err := g.boxExec(r, s, []string{"sh", "-c", readScript, "sh", p, strconv.Itoa(i), strconv.Itoa(fileChunk)}, "", 120, "")
		if err != nil {
			writeZm(w, err)
			return
		}
		switch x.Exit {
		case 0:
		case 3:
			writeErr(w, http.StatusNotFound, "path '%s' does not exist", p)
			return
		case 4:
			writeErr(w, http.StatusBadRequest, "path '%s' is a directory", p)
			return
		case 5:
			writeErr(w, http.StatusForbidden, "path '%s' is not readable", p)
			return
		default:
			writeErr(w, http.StatusInternalServerError, "reading '%s' failed (exit %d)", p, x.Exit)
			return
		}
		if x.Truncated {
			writeErr(w, http.StatusInternalServerError, "reading '%s': a chunk passed potluck sandbox exec's output cap", p)
			return
		}
		line, rest, _ := strings.Cut(x.Stdout, "\n")
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "reading '%s': no size", p)
			return
		}
		if size < 0 {
			if n > fileMax {
				writeErr(w, http.StatusRequestEntityTooLarge, "'%s' is %d bytes; potluck-e2b reads at most %d", p, n, fileMax)
				return
			}
			size = n
		} else if n != size {
			writeErr(w, http.StatusConflict, "'%s' changed while it was read", p)
			return
		}
		chunk, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(rest), ""))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "reading '%s': bad base64", p)
			return
		}
		if len(chunk) == 0 && len(out) < size {
			writeErr(w, http.StatusConflict, "'%s' changed while it was read", p)
			return
		}
		out = append(out, chunk...)
	}
	if len(out) > size {
		writeErr(w, http.StatusConflict, "'%s' changed while it was read", p)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// --- files.write: POST /files (multipart "file" parts, or octet-stream) ---

// writeScript appends base64 stdin to the temp file $2 ($3: "first"
// truncates), and with $4 "last" moves it onto $1 and prints its absolute
// path.
const writeScript = `set -e
p=$1; t=$2; d=$(dirname -- "$p")
mkdir -p -- "$d"
if [ "$3" = first ]; then base64 -d > "$t"; else base64 -d >> "$t"; fi
if [ "$4" = last ]; then
  mv -f -- "$t" "$p"
  cd -- "$d"
  printf '%s/%s\n' "$(pwd -P)" "$(basename -- "$p")"
fi`

type writeInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Path string `json:"path"`
}

func (g *gateway) writeFiles(w http.ResponseWriter, r *http.Request, s *sandbox) {
	q := r.URL.Query().Get("path")
	body := io.Reader(http.MaxBytesReader(w, r.Body, int64(fileMax)+1<<20))
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad gzip body")
			return
		}
		body = zr
	}
	body = &capReader{r: body, n: fileMax} // the decompressed total
	var infos []writeInfo
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		if params["boundary"] == "" {
			writeErr(w, http.StatusBadRequest, "multipart body without a boundary")
			return
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad multipart body: %v", err)
				return
			}
			_, cd, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if cd["name"] != "file" {
				continue
			}
			p := cd["filename"] // the raw name: Part.FileName keeps only its base
			if q != "" {
				p = q
			}
			info, status, err := g.putFile(r, s, p, part)
			if err != nil {
				writeErr(w, status, "%v", err)
				return
			}
			infos = append(infos, info)
		}
	} else {
		info, status, err := g.putFile(r, s, q, body)
		if err != nil {
			writeErr(w, status, "%v", err)
			return
		}
		infos = append(infos, info)
	}
	if len(infos) == 0 {
		writeErr(w, http.StatusBadRequest, "no file in the request")
		return
	}
	writeJSON(w, http.StatusOK, infos)
}

// putFile writes data to p in the box, fileChunk raw bytes per exec, into a
// temp file moved into place at the end (a failed write leaves p as it was).
func (g *gateway) putFile(r *http.Request, s *sandbox, p string, data io.Reader) (writeInfo, int, error) {
	if p == "" {
		return writeInfo{}, http.StatusBadRequest, errors.New("path is required")
	}
	tmp := p + ".potluck-e2b-" + randomID()[:8]
	buf := make([]byte, fileChunk)
	drop := func(i int) { // a failed upload leaves p as it was and no temp file
		if i > 0 {
			_, _ = g.boxExec(r, s, []string{"rm", "-f", "--", tmp}, "", 30, "")
		}
	}
	for i := 0; ; i++ {
		n, last, rerr := readChunk(&data, buf)
		if rerr != nil {
			drop(i)
			var mbe *http.MaxBytesError
			if errors.As(rerr, &mbe) || errors.Is(rerr, errTooLarge) {
				return writeInfo{}, http.StatusRequestEntityTooLarge, fmt.Errorf("potluck-e2b writes at most %d bytes per request", fileMax)
			}
			return writeInfo{}, http.StatusBadRequest, fmt.Errorf("reading the upload: %v", rerr)
		}
		mode, fin := "more", "no"
		if i == 0 {
			mode = "first"
		}
		if last {
			fin = "last"
		}
		x, err := g.boxExec(r, s, []string{"sh", "-c", writeScript, "sh", p, tmp, mode, fin}, base64.StdEncoding.EncodeToString(buf[:n]), 120, "")
		if err != nil {
			drop(i + 1)
			return writeInfo{}, http.StatusServiceUnavailable, fmt.Errorf("potluck: %v", err)
		}
		if x.Exit != 0 {
			drop(i + 1)
			return writeInfo{}, http.StatusBadRequest, fmt.Errorf("writing '%s' failed (exit %d): %s", p, x.Exit, firstLine(x.Stderr))
		}
		if last {
			abs := strings.TrimSpace(x.Stdout)
			if abs == "" {
				abs = p
			}
			return writeInfo{Name: path.Base(abs), Type: "file", Path: abs}, 0, nil
		}
	}
}

// readChunk fills buf from *r. last is true only when the data ended with
// io.EOF (checked by reading one byte past a full buf, kept for the next
// call); any other error, at any point, is returned and the upload fails.
func readChunk(r *io.Reader, buf []byte) (n int, last bool, err error) {
	for n < len(buf) {
		k, rerr := (*r).Read(buf[n:])
		n += k
		if errors.Is(rerr, io.EOF) {
			return n, true, nil
		}
		if rerr != nil {
			return n, false, rerr
		}
	}
	var b [1]byte
	for {
		k, rerr := (*r).Read(b[:])
		if k == 1 {
			*r = io.MultiReader(bytes.NewReader(b[:]), *r)
			return n, false, nil
		}
		if errors.Is(rerr, io.EOF) {
			return n, true, nil
		}
		if rerr != nil {
			return n, false, rerr
		}
	}
}

// firstLine quotes a box's words for an error: one line, at most 200 bytes.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strconv.Quote(s)
}

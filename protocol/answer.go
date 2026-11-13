package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// MaxAnswer is the most stdout a driver's answer may hold (an exec answer
// holds up to 2 MiB of output, JSON-escaped).
const MaxAnswer = 16 << 20

// AnswerBuffer is where a driver's stdout goes. It keeps up to MaxAnswer+1
// bytes and drops the rest, so ReadAnswer sees an answer that is too long as
// too long and never as a cut one.
type AnswerBuffer struct{ b bytes.Buffer }

func (a *AnswerBuffer) Write(p []byte) (int, error) {
	if room := MaxAnswer + 1 - a.b.Len(); room > 0 {
		a.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// Bytes is what was written, up to MaxAnswer+1 bytes.
func (a *AnswerBuffer) Bytes() []byte { return a.b.Bytes() }

// KnownCode says whether c is one of the error codes a driver may answer
// (docs/protocol.md, "Error codes").
func KnownCode(c string) bool {
	switch c {
	case CodeBadRequest, CodeNotFound, CodeConflict, CodeForbidden,
		CodeNotImplemented, CodeUnavailable, CodeInternal:
		return true
	}
	return false
}

// Answer is a driver's reply to one request, read by ReadAnswer: either OK
// with Result, or a refusal with Error.
type Answer struct {
	OK     bool
	Result json.RawMessage
	Error  *Error
}

// ReadAnswer is the one rule for a driver's answer; the daemon and the
// conformance suite both apply it. exit is the driver's run error (nil when it
// exited 0), run with exec.Cmd.WaitDelay set. The driver must exit 0, also for
// a refusal, and leave nothing running that holds its stdout or stderr open
// (exec.ErrWaitDelay). Its stdout, an AnswerBuffer's bytes, must hold exactly
// one JSON value of at most MaxAnswer bytes: an object with "ok"; ok:true
// carries "result", ok:false carries an error with a known code and a message.
// Anything else is an error, and the answer is not used.
func ReadAnswer(stdout []byte, exit error) (Answer, error) {
	if errors.Is(exit, exec.ErrWaitDelay) {
		return Answer{}, errors.New("something the driver started kept stdout or stderr open past its exit")
	}
	if exit != nil {
		return Answer{}, fmt.Errorf("the driver did not run to exit 0 (%v); every answer exits 0", exit)
	}
	if len(stdout) > MaxAnswer {
		return Answer{}, fmt.Errorf("answer over %d bytes", MaxAnswer)
	}
	var resp struct { // only what the rule reads: a stray field of another type is no reason to refuse
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&resp); err != nil {
		return Answer{}, fmt.Errorf("stdout is not one JSON answer: %s", decodeProblem(err))
	}
	if rest := bytes.TrimSpace(stdout[dec.InputOffset():]); len(rest) > 0 {
		return Answer{}, errors.New("stdout has more than one answer: human text goes to stderr")
	}
	switch {
	case resp.OK == nil:
		return Answer{}, errors.New(`answer has no "ok"`)
	case !*resp.OK && (resp.Error == nil || resp.Error.Code == "" || resp.Error.Message == ""):
		return Answer{}, errors.New("ok:false without error.code and error.message")
	case !*resp.OK && !KnownCode(resp.Error.Code):
		return Answer{}, errors.New("the error code is not a known code") // the code is driver text: never echoed
	case !*resp.OK:
		return Answer{Error: &Error{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}}, nil
	case len(resp.Result) == 0:
		return Answer{}, errors.New(`ok:true without "result"`)
	}
	return Answer{OK: true, Result: resp.Result}, nil
}

// decodeProblem says what is wrong with a stdout that did not decode, in
// words about the answer. It never names a Go type, and quotes at most one
// character of the driver's text (a syntax error does), so it is short.
func decodeProblem(err error) string {
	var wrong *json.UnmarshalTypeError
	switch {
	case !errors.As(err, &wrong):
		return err.Error()
	case wrong.Field == "":
		return fmt.Sprintf("it is a JSON %s, not an object", wrong.Value)
	default:
		return fmt.Sprintf("%q is a JSON %s, which is the wrong kind of value there", wrong.Field, wrong.Value)
	}
}

// DecodeResult reads an answer's result into out. A result that does not fit
// is an error whose text is bounded: the decoder can quote a driver's number.
func DecodeResult(result json.RawMessage, out any) error {
	err := json.Unmarshal(result, out)
	if err == nil {
		return nil
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = strings.ToValidUTF8(msg[:120], "") + "…"
	}
	return errors.New(msg)
}

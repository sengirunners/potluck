package protocol_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/protocol"
)

func TestReadAnswer(t *testing.T) {
	exit1 := errors.New("exit status 1")
	for name, c := range map[string]struct {
		stdout string
		exit   error
		ok     bool   // a usable success
		code   string // a usable refusal
		says   string // else the error says this
	}{
		"success":              {stdout: `{"ok":true,"result":{"a":1}}` + "\n", ok: true},
		"success, null result": {stdout: `{"ok":true,"result":null}`, ok: true},
		"refusal":              {stdout: `{"ok":false,"error":{"code":"not_found","message":"no box"}}`, code: "not_found"},
		"blank lines around":   {stdout: "\n " + `{"ok":true,"result":1}` + "\n\n", ok: true},
		"non-zero exit":        {stdout: `{"ok":true,"result":1}`, exit: exit1, says: "every answer exits 0"},
		"refusal, non-zero":    {stdout: `{"ok":false,"error":{"code":"internal","message":"x"}}`, exit: exit1, says: "every answer exits 0"},
		"text before":          {stdout: "hello\n" + `{"ok":true,"result":1}`, says: "not one JSON answer"},
		"text after":           {stdout: `{"ok":true,"result":1}` + "\nbye\n", says: "more than one answer"},
		"two answers":          {stdout: `{"ok":true,"result":1}` + "\n" + `{"ok":true,"result":2}`, says: "more than one answer"},
		"empty":                {stdout: "", says: "not one JSON answer"},
		"no ok":                {stdout: `{"result":1}`, says: `no "ok"`},
		"ok without result":    {stdout: `{"ok":true}`, says: `without "result"`},
		"refusal without code": {stdout: `{"ok":false,"error":{"message":"x"}}`, says: "error.code and error.message"},
		"refusal without msg":  {stdout: `{"ok":false,"error":{"code":"internal"}}`, says: "error.code and error.message"},
		"refusal, no error":    {stdout: `{"ok":false}`, says: "error.code and error.message"},
		"error is a string":    {stdout: `{"ok":false,"error":"x"}`, says: `"error" is a JSON string`},
		"code is a number":     {stdout: `{"ok":false,"error":{"code":1` + strings.Repeat("0", 500) + `,"message":"x"}}`, says: `"error.code" is a JSON number`},
		"answer is an array":   {stdout: `[1]`, says: "it is a JSON array, not an object"},
		"held pipe":            {stdout: `{"ok":true,"result":1}`, exit: fmt.Errorf("wait: %w", exec.ErrWaitDelay), says: "kept stdout or stderr open"},
		"at the cap":           {stdout: `{"ok":true,"result":"` + strings.Repeat("a", protocol.MaxAnswer-len(`{"ok":true,"result":""}`)) + `"}`, ok: true},
		"over the cap":         {stdout: `{"ok":true,"result":"` + strings.Repeat("a", protocol.MaxAnswer) + `"}`, says: "answer over"},
		"unknown code":         {stdout: `{"ok":false,"error":{"code":"teapot","message":"x"}}`, says: "not a known code"},
	} {
		a, err := protocol.ReadAnswer([]byte(c.stdout), c.exit)
		if err != nil && len(err.Error()) > 200 {
			t.Errorf("%s: error of %d bytes, want it bounded", name, len(err.Error()))
		}
		if err != nil && strings.Contains(err.Error(), "struct") {
			t.Errorf("%s: %q names a Go type", name, err)
		}
		switch {
		case c.says != "":
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Errorf("%s: %+v %v, want an error saying %q", name, a, err, c.says)
			}
		case err != nil:
			t.Errorf("%s: %v", name, err)
		case c.ok && (!a.OK || a.Error != nil || len(a.Result) == 0):
			t.Errorf("%s: %+v, want a success", name, a)
		case !c.ok && (a.OK || a.Error == nil || a.Error.Code != c.code):
			t.Errorf("%s: %+v, want refusal %s", name, a, c.code)
		}
	}
}

// TestAnswerBufferKeepsOneByteOverTheCap: a stdout past the cap reaches
// ReadAnswer as too long, never as an answer cut short.
func TestAnswerBufferKeepsOneByteOverTheCap(t *testing.T) {
	var b protocol.AnswerBuffer
	chunk := []byte(strings.Repeat("x", 1<<20))
	for i := 0; i < protocol.MaxAnswer>>20+2; i++ {
		if n, err := b.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("write: %d, %v", n, err)
		}
	}
	if len(b.Bytes()) != protocol.MaxAnswer+1 {
		t.Fatalf("kept %d bytes, want %d", len(b.Bytes()), protocol.MaxAnswer+1)
	}
	if _, err := protocol.ReadAnswer(b.Bytes(), nil); err == nil || !strings.Contains(err.Error(), "answer over") {
		t.Fatalf("ReadAnswer: %v", err)
	}
}

func TestDecodeResultIsBounded(t *testing.T) {
	var n int
	if err := protocol.DecodeResult(json.RawMessage("7"), &n); err != nil || n != 7 {
		t.Fatalf("%d, %v", n, err)
	}
	err := protocol.DecodeResult(json.RawMessage(`1`+strings.Repeat("0", 500)), &n)
	if err == nil || len(err.Error()) > 130 {
		t.Fatalf("want a short error, got %v", err)
	}
}

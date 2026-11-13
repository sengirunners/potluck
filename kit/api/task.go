package api

import "encoding/json"

// MachineOp is one operation the authority queued for a machine, carried in
// its heartbeat answer (or its ops poll) and run there at most once (SPEC 8
// "Machine choice"). Kind names a handler the module that owns it
// registered with teamlink; Payload is that kind's request, opaque to the
// queue.
type MachineOp struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	From    string          `json:"from,omitempty"` // the identity that asked
	Payload json.RawMessage `json:"payload,omitempty"`
}

// MachineOpReport is a machine's word on an op: the result of running it
// (Payload, the kind's answer), refreshed while it is not final (a started
// task's latest status). Error is a refusal, Code and Hint its api error's.
type MachineOpReport struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind,omitempty"`
	Error   string          `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
	Hint    string          `json:"hint,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

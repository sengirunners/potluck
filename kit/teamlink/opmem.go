package teamlink

// OpMem keeps what a relayed machine op must never write to the queue's
// table: its arguments (until the answer carrying the op takes them) and
// its result (until the caller waiting on it reads it), in memory, by op
// id. A relay puts the arguments before QueueOp, its OpHandler.Args reads
// them, its Apply finishes the call and the waiter reads Result once Done.

import (
	"encoding/json"
	"sync"

	"github.com/sengirunners/potluck/kit/api"
)

// OpMem is the in-memory side of relayed ops (the zero value is ready).
type OpMem struct {
	mu sync.Mutex
	by map[string]*OpCall
}

// OpCall is one relayed op waiting for its result.
type OpCall struct {
	args json.RawMessage
	res  *OpResult
	done chan struct{}
}

// OpResult is a relayed op's outcome: Raw, or a failure (Code, Msg, Hint).
type OpResult struct {
	Raw  json.RawMessage
	Code string
	Msg  string
	Hint string
	Fail bool
}

// Err is r's failure as an *api.Error (unavailable when it has no code);
// nil when r did not fail.
func (r OpResult) Err() error {
	if !r.Fail {
		return nil
	}
	code := r.Code
	if code == "" {
		code = api.CodeUnavailable
	}
	return &api.Error{Code: code, Message: r.Msg, Hint: r.Hint}
}

// Put holds args for op id and answers its call.
func (m *OpMem) Put(id string, args json.RawMessage) *OpCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]*OpCall{}
	}
	c := &OpCall{args: args, done: make(chan struct{})}
	m.by[id] = c
	return c
}

// Args are op id's arguments; ok false when no call waits for it (it
// finished, or this daemon restarted since).
func (m *OpMem) Args(id string) (args json.RawMessage, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.by[id]
	if c == nil {
		return nil, false
	}
	return c.args, true
}

// Forget drops op id (the waiter is done with it).
func (m *OpMem) Forget(id string) {
	m.mu.Lock()
	delete(m.by, id)
	m.mu.Unlock()
}

// Finish records op id's result once and wakes its waiter.
func (m *OpMem) Finish(id string, r OpResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.by[id]; c != nil && c.res == nil {
		c.res, c.args = &r, nil
		close(c.done)
	}
}

// Done is closed once the call has its result.
func (c *OpCall) Done() <-chan struct{} { return c.done }

// Result is the call's result (after Done).
func (c *OpCall) Result() OpResult { return *c.res }

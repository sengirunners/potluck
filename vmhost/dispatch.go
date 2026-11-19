package vmhost

import (
	"encoding/json"
	"slices"

	"github.com/sengirunners/potluck/protocol"
)

func dispatch(verb, id string, args json.RawMessage, m map[string]string) (any, error) {
	known := []string{"describe", "acquire", "status", "release", "list", "renew", protocol.VerbProgress, protocol.VerbCancel, "exec", "spawn", "output", "signal", "input", "stop", "start", "secrets", "port"}
	boxVerb := slices.Contains(known[8:], verb)
	if !slices.Contains(known, verb) {
		return nil, refuse("not_implemented", "verb %q not supported", verb)
	}
	cfg, err := parseConfig(m)
	if err != nil {
		return nil, err
	}
	if boxVerb && cfg.Kind != "box" {
		return nil, refuse("not_implemented", "%s is a box's verb", verb)
	}
	d := &drv{cfg: cfg, st: store{cfg.StateDir}, vm: newLayer(cfg)}
	if err := d.st.create(); err != nil {
		return nil, err
	}
	switch verb { // progress.go: never behind the sweep's lock
	case protocol.VerbProgress:
		return d.progress(args)
	case protocol.VerbCancel:
		return d.cancel(args)
	}
	var box record
	if id != "" {
		var err error
		if box, err = d.find(id); err != nil {
			return nil, err
		}
	}
	if verb == "acquire" { // progress.go: progress knows it before the sweep's lock
		return d.acquireTracked(args, func() error { return d.prepare(verb) })
	}
	if !d.skipsSweep(verb, box) {
		if err := d.prepare(verb); err != nil {
			return nil, err
		}
	}
	switch verb {
	case "describe":
		return d.describe(), nil
	case "status":
		return d.status(id)
	case "release":
		return d.release(id, args)
	case "list":
		return d.list(), nil
	case "port":
		return d.port(id, args)
	case "exec":
		return d.exec(id, args)
	case "spawn":
		return d.spawn(id, args)
	case "output":
		return d.output(id, args)
	case "signal":
		return d.signal(id, args)
	case "input":
		return d.input(id, args) // pty.go
	case "stop":
		return d.stop(id) // persist.go
	case "start":
		return d.start(id, args)
	case "secrets":
		return d.boxSecrets(id, args) // secrets.go
	}
	return d.renew(id, args)
}

// prepare readies d for verb: an OCI acquire's host budget and engine, then
// the sweep, under the state lock.
func (d *drv) prepare(verb string) error {
	if verb == "acquire" && d.cfg.VM == "oci" {
		if _, err := d.requestedHostBudget(); err != nil {
			return refuse("unavailable", "%s", err)
		}
		o := d.vm.(ociCLI)
		o.engineHost = d.engine.endpoint
		d.vm = o
	}
	return d.st.locked(d.sweep)
}

// skipsSweep says whether verb needs no sweep first: exec and output on a
// ready box whose supervisor runs. The sweep takes the state lock and asks
// the VM layer what it holds (an engine check and a list, some 0.5 s a call
// on a Docker host), which would make every exec of a file copy wait in line
// behind the others; it changes nothing about a box whose supervisor is
// alive, and a box whose supervisor died still sweeps, so it answers as lost.
// A box whose record names no engine (made before records did) sweeps too:
// the sweep is what picks the engine for it.
func (d *drv) skipsSweep(verb string, r record) bool {
	if verb != "exec" && verb != "output" {
		return false
	}
	if r.State != stReady || !alive(r.Supervisor) {
		return false
	}
	_, oci := d.vm.(ociCLI)
	return !oci || r.Engine != nil
}

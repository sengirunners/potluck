package runtarget

import "github.com/sengirunners/potluck/kit/core"

// Routes registers /v0/run_targets.
func (s *Service) Routes(r *core.Router) {
	s.initPorts()
	s.previewState()
	r = r.For("runtarget")
	r.HandleFunc("GET /v0/run_targets/{target}/handles/{id}/previews", core.Authed, "sandbox holder lists its published previews", s.handle(s.previews))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/previews", core.Authed, "sandbox holder explicitly publishes or revokes its HTTP port", s.handle(s.publishPreview))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/port", core.Authed, "operate on the resolved project; sandbox holder, guest loopback only", s.handle(s.port))
	r.HandleFunc("GET /v0/run_targets", core.Authed, "read on the resolved project; ?describe=1 asks each driver", s.handle(s.list))
	r.HandleFunc("POST /v0/run_targets", core.Local, "machine owner only: admin on the resolved project; describe must answer", s.handle(s.bind))
	r.HandleFunc("DELETE /v0/run_targets/{target}", core.Local, "machine owner only: admin on the resolved project; refused while handles are live", s.handle(s.unbind))
	r.HandleFunc("GET /v0/run_targets/{target}", core.Authed, "read on the resolved project", s.handle(s.describe))
	r.HandleFunc("POST /v0/run_targets/{target}/acquire", core.Authed, "operate on the resolved project; a db, credentialed or unisolated target: machine owner only", s.handle(s.acquire))
	r.HandleFunc("GET /v0/run_targets/{target}/acquires/{request_id}", core.Authed, "read on the resolved project; the caller's own request_id", s.handle(s.acquireProgress))
	r.HandleFunc("POST /v0/run_targets/{target}/acquires/{request_id}/cancel", core.Authed, "operate on the resolved project; the caller's own request_id", s.handle(s.cancelAcquire))
	r.HandleFunc("GET /v0/run_targets/{target}/handles", core.Authed, "read on the resolved project; all handles for the machine owner or a whole-project admin, else only the caller's own", s.handle(s.handles))
	r.HandleFunc("GET /v0/run_targets/{target}/handles/{id}", core.Authed, "read on the resolved project; another member's handle answers not_found (as the list; its operate routes still answer held by another actor to a caller who has its random id)", s.handle(s.status))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/release", core.Authed, "operate on the resolved project; the holder, or the machine owner", s.handle(s.release))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/renew", core.Authed, "operate on the resolved project; the holder", s.handle(s.renew))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/exec", core.Authed, "operate on the resolved project; a box's holder", s.handle(s.exec))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/spawn", core.Authed, "operate on the resolved project; a box's holder", s.handle(s.spawn))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/output", core.Authed, "operate on the resolved project; a box's holder", s.handle(s.output))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/signal", core.Authed, "operate on the resolved project; a box's holder", s.handle(s.signal))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/input", core.Authed, "operate on the resolved project; a box's holder", s.handle(s.input))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/stop", core.Authed, "operate on the resolved project; a persistent box's holder, or the machine owner", s.handle(s.stop))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/start", core.Authed, "operate on the resolved project; a stopped box's holder", s.handle(s.start))
	r.HandleFunc("GET /v0/sealing", core.Authed, "read on the resolved project: its sealing mode and the machines heard from", s.handle(s.getSealing))
	r.HandleFunc("POST /v0/sealing/forget", core.Local, "this machine's owner only: forget that a pool's machine named a seal key", s.handle(s.forgetSealedHome))
	r.HandleFunc("PUT /v0/sealing", core.Authed, "admin on the resolved project; required refused while a machine heard from cannot seal, unless force", s.handle(s.setSealing))
	r.HandleFunc("POST /v0/run_targets/{target}/handles/{id}/secrets", core.Authed, "operate on the resolved project; a box's holder, on a trusted host, under a secrets grant", s.handle(s.boxSecrets))
}

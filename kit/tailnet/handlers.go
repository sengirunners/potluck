package tailnet

import (
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// Every route here is core.Local: the router has already refused token and
// network callers, so the handlers only do the work.

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, s.Status(r.Context()))
}

func (s *Service) handleUp(w http.ResponseWriter, r *http.Request) {
	var req api.TailnetUpRequest
	if r.Body == nil { // in-process callers may send none
		r.Body = http.NoBody
	}
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if _, err := s.Up(r.Context(), req); err != nil {
		api.WriteError(w, err)
		return
	}
	st := s.waitStatus(r.Context(), upWait)
	if st.Error != "" && !st.Running {
		// The node started but cannot serve (e.g. the listener failed):
		// never a 200, and nothing saved. The next up starts it over.
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "tailnet: %s", st.Error).
			WithHint("run "+config.Command("tailnet up")+" again to retry, or "+config.Command("tailnet down")))
		return
	}
	// Saved only once the node started, so a bad auth key or control URL
	// does not leave the daemon retrying on every restart.
	if err := s.saveEnabled(true, req); err != nil {
		s.d.Log.Warn("tailnet: save config", "err", err)
		api.WriteError(w, api.Errorf(api.CodeInternal, "the node is up but config.json was not saved: %v", err).
			WithHint("it will not come back after a daemon restart; fix config.json and run "+config.Command("tailnet up")+" again"))
		return
	}
	api.WriteJSON(w, http.StatusOK, st)
}

func (s *Service) handleDown(w http.ResponseWriter, r *http.Request) {
	if err := s.saveEnabled(false, api.TailnetUpRequest{}); err != nil {
		s.d.Log.Warn("tailnet: save config", "err", err)
		api.WriteError(w, api.Errorf(api.CodeInternal, "config.json was not saved: %v", err).
			WithHint("the node is still up; fix config.json and run "+config.Command("tailnet down")+" again"))
		return
	}
	api.WriteJSON(w, http.StatusOK, s.Down(r.Context()))
}

func (s *Service) handlePeers(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, api.PeerList{Peers: s.Peers(r.Context())})
}

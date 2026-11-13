package tunnel

import (
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"net/http"
)

const ServiceName = "kit.tunnel"

func From(d *core.Deps) *Service         { s, _ := d.Service(ServiceName).(*Service); return s }
func (s *Service) PreviewDomain() string { return s.cfg().PreviewDomain }

func (s *Service) handlePreviewDomain(w http.ResponseWriter, r *http.Request) {
	var req api.TunnelPreviewDomainRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if req.Domain != "" && (!core.ValidPublicSuffix(req.Domain) || len(req.Domain) > 204) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "domain must be a lowercase DNS domain of at most 204 characters"))
		return
	}
	if err := s.update(func(c *config.Tunnel) { c.PreviewDomain = req.Domain }); err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.Status())
}

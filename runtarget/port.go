package runtarget

import (
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
	"net/http"
	"regexp"
)

var portID = regexp.MustCompile(`^[0-9a-f]{48}$`)

func (s *Service) port(r *http.Request) (any, error) {
	var req protocol.PortRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	return s.portRequest(r, req)
}

func validatePort(req protocol.PortRequest) error {
	if len(req.Data) > protocol.PortPage {
		return api.Errorf(api.CodeBadRequest, "port page exceeds 64 KiB")
	}
	switch req.Op {
	case "open":
		if req.Port > 0 && req.Port <= 65535 && req.Stream == "" && len(req.Data) == 0 {
			return nil
		}
	case "read", "write", "close":
		if portID.MatchString(req.Stream) && req.Port == 0 && (req.Op == "write" || len(req.Data) == 0) {
			return nil
		}
	}
	return api.Errorf(api.CodeBadRequest, "invalid port operation")
}

package vmhost

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/boxport"
	"github.com/sengirunners/potluck/protocol"
	"io"
	"os/exec"
)

// port connects only inside the guest; readyBox owns the lease check.
func (d *drv) port(handle string, raw json.RawMessage) (any, error) {
	var req protocol.PortRequest
	if json.Unmarshal(raw, &req) != nil {
		return nil, refuse("bad_request", "invalid port request")
	}
	r, err := d.readyBox(handle)
	if err != nil {
		return nil, err
	}
	out, err := boxport.Exchange(context.Background(), req, r.Deadline, func(in io.Reader, out, errout io.Writer, argv ...string) *exec.Cmd {
		return d.vm.Exec(r.VM, in, out, errout, argv...)
	})
	if err != nil {
		if e, ok := err.(*protocol.Error); ok {
			return nil, refuse(e.Code, "%s", e.Message)
		}
	}
	return out, err
}

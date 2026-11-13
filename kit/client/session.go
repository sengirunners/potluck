package client

// The session calls moved to zm's internal/orchclient (zm-eakh.12.35.11);
// GET /v0/machines is KIT (teamlink answers it) and stays.

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
)

// Machines is GET /v0/machines: this machine's numbers and, on a team, each
// member machine's (SPEC 8 "Machine numbers").
func (c *Client) Machines(ctx context.Context) (api.MachineList, error) {
	var l api.MachineList
	return l, c.Do(ctx, http.MethodGet, "/v0/machines", nil, &l)
}

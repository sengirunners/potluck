package runtarget

import (
	"context"
	"database/sql"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
)

type databaseContextKey struct{}

// prepareDatabaseLayers resolves the task image before computing dependency
// keys. Keep it through this acquire's build loop; a durable replay instead
// reads the first acquire's stored identity, just as the final box does.
func (s *Service) prepareDatabaseLayers(ctx context.Context, t core.Target, b computeapi.RunTarget, db *sql.DB, req computeapi.AcquireRunTargetRequest) (context.Context, error) {
	if req.Database == "" {
		return ctx, nil
	}
	actor, err := actingFor(ctx, req.Actor)
	if err != nil {
		return ctx, err
	}
	prev, seen, err := byRequest(ctx, db, b.Name, actorKey(actor, req.RequestID))
	if err != nil {
		return ctx, err
	}
	if seen && rowHidden(ctx, t.Project.ID, prev) {
		return ctx, api.NotFound("handle", req.RequestID)
	}
	snapshot, err := s.databaseFor(ctx, t.Project.ID, req, prev.request, seen)
	if err != nil {
		return ctx, err
	}
	if err := s.hostTrusted(ctx, b, "database snapshot"); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, databaseContextKey{}, snapshot), nil
}

func databaseLayerImage(ctx context.Context, req computeapi.AcquireRunTargetRequest) string {
	if req.Database != "" {
		if d, ok := ctx.Value(databaseContextKey{}).(*protocol.Database); ok && d.Name == req.Database {
			return d.Image
		}
	}
	return ""
}

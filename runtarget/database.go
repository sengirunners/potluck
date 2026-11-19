package runtarget

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/database"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
)

func checkDatabaseRequest(b computeapi.RunTarget, req computeapi.AcquireRunTargetRequest) error {
	if req.Database == "" {
		return nil
	}
	if b.Kind != computeapi.RunTargetKindBox || req.Persist || req.JITConfig != "" || req.Image != "" || req.Template != "" {
		return api.Errorf(api.CodeBadRequest, "database is for a fresh box without persist, runner, image or template")
	}
	if req.Base != nil && req.Base.Fill {
		return api.Errorf(api.CodeBadRequest, "a push-base builder takes no database; the task mounts the sealed base afterwards")
	}
	for _, v := range req.Volumes {
		if protocol.VolumePathsOverlap(protocol.DatabaseHome, v.Path) {
			return api.Errorf(api.CodeBadRequest, "volume %s overlaps private database directory", v.Name)
		}
	}
	for _, layer := range req.Deps {
		if protocol.VolumePathsOverlap(protocol.DatabaseHome, layer.Path) {
			return api.Errorf(api.CodeBadRequest, "dependency layer overlaps private database directory")
		}
	}
	return nil
}

// databaseFor freezes the first resolved recipe in the acquire's durable
// fingerprint. A retry never resolves a changed project file into a new image.
func (s *Service) databaseFor(ctx context.Context, projectID string, req computeapi.AcquireRunTargetRequest, previous string, seen bool) (*protocol.Database, error) {
	if req.Database == "" {
		return nil, nil
	}
	if seen {
		var p struct {
			Database       string
			DatabaseRecipe *protocol.Database
		}
		if json.Unmarshal([]byte(previous), &p) != nil || p.Database != req.Database || p.DatabaseRecipe == nil || p.DatabaseRecipe.Project != projectID {
			return nil, api.Errorf(api.CodeConflict, "request_id was used for a different database")
		}
		if _, err := database.Check(p.DatabaseRecipe); err != nil {
			return nil, api.Errorf(api.CodeInternal, "stored database recipe is invalid")
		}
		return p.DatabaseRecipe, nil
	}
	if d, ok := ctx.Value(databaseContextKey{}).(*protocol.Database); ok && d.Project == projectID && d.Name == req.Database {
		return d, nil
	}
	d, err := database.Resolve(s.d.Projects, projectID, req.Database)
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "%v", err)
	}
	return d, nil
}

func checkDatabaseDriver(d *protocol.Database, caps []string) error {
	if d != nil && !slices.Contains(caps, protocol.CapDatabase) {
		return api.Errorf(api.CodeBadRequest, "driver has no %s capability", protocol.CapDatabase)
	}
	return nil
}
func databaseAnswer(want, got *protocol.Database) string {
	if want != nil && (got == nil || *want != *got) {
		return "did not confirm the requested database provenance"
	}
	if want == nil && got != nil {
		return fmt.Sprintf("answered an unrequested database %q", got.Name)
	}
	return ""
}

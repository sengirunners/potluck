package runtarget

// Push bases are holder-owned sealed Git object stores. The daemon chooses
// their project/repository/week name; the volume driver supplies owner
// isolation, one builder, immutability and eviction.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/protocol"
)

const (
	basePrefix    = "base-"
	basePath      = ".potluck-base"
	baseBuildTTLS = 900
)

// baseMount is persisted in the daemon's request fingerprint. Only the
// daemon creates it; a caller's request ID never grants permission to seal.
type baseMount struct {
	Repo string `json:"repo"`
	Name string `json:"name"`
	Fill bool   `json:"fill,omitempty"`
}

func baseName(project, repo string, at time.Time) string {
	sum := sha256.Sum256([]byte(project + "\x00" + repo))
	year, week := at.UTC().ISOWeek()
	return fmt.Sprintf("base-git-%x-%04d%02d", sum[:20], year, week)
}

func checkBase(req computeapi.AcquireRunTargetRequest) error {
	if req.Base == nil {
		return nil
	}
	key, err := hex.DecodeString(req.Base.Repo)
	if err != nil || len(key) != sha256.Size || req.Base.Repo != strings.ToLower(req.Base.Repo) {
		return api.Errorf(api.CodeBadRequest, "base.repo is a lowercase SHA256 repository key")
	}
	if req.Base.Fill { // the builder takes none of these settings
		return nil
	}
	if len(req.Volumes)+len(req.Deps)+1 > computeapi.VolumesMax {
		return api.Errorf(api.CodeBadRequest, "at most %d volumes, dependency layers and push base together", computeapi.VolumesMax)
	}
	for _, v := range req.Volumes {
		if protocol.VolumePathsOverlap(basePath, v.Path) {
			return api.Errorf(api.CodeBadRequest, "volume %s overlaps the push base at %s", v.Name, basePath)
		}
	}
	for _, d := range req.Deps {
		if protocol.VolumePathsOverlap(basePath, d.Path) {
			return api.Errorf(api.CodeBadRequest, "dependency layer overlaps the push base at %s", basePath)
		}
	}
	return nil
}

// acquireBox composes the base with whatever dependency volumes the caller
// has prepared. The deps loop keeps req.Base on every retry, including its
// optional acquire without dependencies.
func (s *Service) acquireBox(ctx context.Context, t core.Target, b computeapi.RunTarget, db *sql.DB, req computeapi.AcquireRunTargetRequest, x daemonVols) (any, error) {
	if req.Base == nil {
		return s.acquireBoxWithVolumes(ctx, t, b, db, req, x)
	}
	if err := checkBase(req); err != nil {
		return nil, err
	}
	if b.Kind != computeapi.RunTargetKindBox {
		return nil, api.Errorf(api.CodeBadRequest, "base is for a box target")
	}
	ask := *req.Base
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return nil, err
	}
	why := s.baseUnavailable(ctx, b, d, req)
	if why != "" {
		if ask.Fill {
			return nil, api.Errorf(api.CodeBadRequest, "cannot fill a push base: %s", why)
		}
		req.Base = nil
		res, err := s.acquireBoxWithVolumes(ctx, t, b, db, req, x)
		return withBase(res, computeapi.BoxBaseAnswer{Why: why}), err
	}
	mount := &baseMount{Repo: ask.Repo, Name: baseName(t.Project.ID, ask.Repo, s.now()), Fill: ask.Fill}
	vol := computeapi.BoxVolume{Name: mount.Name, Path: basePath, Mode: computeapi.VolumeRO, Sealed: true}
	if ask.Fill {
		if req.TTLS < 0 {
			return nil, api.Errorf(api.CodeBadRequest, "ttl_s must not be negative")
		}
		ttl := baseBuildTTLS
		if req.TTLS > 0 {
			ttl = min(ttl, req.TTLS)
		}
		if cap := lifetimeCap(b, d); cap > 0 {
			ttl = min(ttl, cap)
		}
		req = computeapi.AcquireRunTargetRequest{RequestID: req.RequestID, Actor: req.Actor,
			Image: req.Image, Template: req.Template, TTLS: ttl, Net: computeapi.NetOff, SelfRenew: true}
		x = daemonVols{}
		vol.Mode, vol.Sealed = protocol.VolumeBuild, false
	}
	// First among daemon volumes: a missing base refuses before any layer
	// build is considered. The error still identifies its precise volume.
	x.vols = append([]computeapi.BoxVolume{vol}, x.vols...)
	x.base = mount
	req.Base = nil
	res, err := s.acquireBoxWithVolumes(ctx, t, b, db, req, x)
	if err != nil && !ask.Fill && mount.Name != "" && missingVolume(err) == mount.Name {
		return nil, api.Errorf(api.CodeNotFound, "the push base is not sealed on run target %s", b.Name).
			WithData(computeapi.BaseRefusal{Reason: computeapi.BaseMissing})
	}
	if err != nil {
		return nil, err
	}
	if mount.Name == "" {
		return withBase(res, computeapi.BoxBaseAnswer{Why: "this request already acquired a box without a base"}), nil
	}
	return withBase(res, computeapi.BoxBaseAnswer{Path: basePath, Hit: !ask.Fill}), nil
}

func (s *Service) baseUnavailable(ctx context.Context, b computeapi.RunTarget, d *computeapi.RunTargetDescribe, req computeapi.AcquireRunTargetRequest) string {
	if !req.Base.Fill && (req.Persist || req.JITConfig != "") {
		return "persistent boxes and GitHub runners do not mount push bases"
	}
	if err := s.hostTrusted(ctx, b, "push base"); err != nil {
		return errText(err)
	}
	for _, cap := range []string{protocol.CapVolumes, protocol.CapVolumesSeal, protocol.CapVolumesKeep, computeapi.CapNetOff} {
		if !slices.Contains(d.Capabilities, cap) {
			return "the binding does not support " + cap
		}
	}
	return ""
}

func withBase(res any, ans computeapi.BoxBaseAnswer) any {
	if h, ok := res.(computeapi.RunTargetHandle); ok {
		h.Base = &ans
		return h
	}
	return res
}

// confirm refuses silent downgrades by an older remote host or driver. A
// mount is reported only after the driver confirms its exact properties.
func (m *baseMount) confirm(vols []computeapi.BoxVolume) string {
	mode := computeapi.VolumeRO
	if m.Fill {
		mode = protocol.VolumeBuild
	}
	for _, v := range vols {
		if v.Name == m.Name && v.Path == basePath && v.Mode == mode && v.Sealed == !m.Fill && !v.Checkout {
			return ""
		}
	}
	return "did not confirm the push base mount"
}

// replayBase retains the original epoch for an identical request. The
// remaining fingerprint still verifies all caller settings under the lock.
func (x *daemonVols) replayBase(raw string) {
	if x.base == nil {
		return
	}
	var saved struct{ Base *baseMount }
	if json.Unmarshal([]byte(raw), &saved) != nil {
		return
	}
	if saved.Base == nil && !x.base.Fill {
		// A base is an optional optimization. A retry after client fallback
		// must keep the already allocated plain box, not add a new mount.
		name := x.base.Name
		*x.base = baseMount{}
		x.base = nil
		x.vols = slices.DeleteFunc(x.vols, func(v computeapi.BoxVolume) bool { return v.Name == name && v.Path == basePath })
		return
	}
	if saved.Base == nil || saved.Base.Repo != x.base.Repo || saved.Base.Fill != x.base.Fill {
		return
	}
	current := x.base.Name
	*x.base = *saved.Base
	for i := range x.vols {
		if x.vols[i].Name == current && x.vols[i].Path == basePath {
			x.vols[i].Name = saved.Base.Name
		}
	}
}

func baseBuilder(rw row) bool {
	var saved struct{ Base *baseMount }
	return json.Unmarshal([]byte(rw.request), &saved) == nil && saved.Base != nil && saved.Base.Fill
}

func baseRelease(ctx context.Context, rw row, req computeapi.ReleaseRunTargetRequest) (any, error) {
	if !req.Seal {
		return nil, nil
	}
	if !baseBuilder(rw) {
		return nil, api.Errorf(api.CodeBadRequest, "seal is only for a push base builder")
	}
	if err := holder(ctx, rw, false); err != nil {
		return nil, err
	}
	return map[string]any{"seal": true, "keep": []string{"objects", "refs"}}, nil
}

func isBaseMissing(err error) bool {
	var e *api.Error
	var data computeapi.BaseRefusal
	return errors.As(err, &e) && e.Code == api.CodeNotFound && json.Unmarshal(e.Data, &data) == nil && data.Reason == computeapi.BaseMissing
}

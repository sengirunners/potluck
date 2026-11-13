package rt

// Machine-level box bindings (SPEC 20 "Across machines"): $ZM_HOME/
// run_targets.toml holds format = 2 (HostFileFormat), then [[run_targets]]
// tables like project.toml's, kind box only, each with
// share = ["<team project id>", ...]: the projects whose
// authority may see the binding and have its driver verbs forwarded here. The
// file is written only by the machine's owner (through the daemon). A host
// binding names no repo: refs are resolved against a project's remotes, which
// a machine-level binding does not have.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

// HostFileFormat is the format line of a run_targets.toml this build
// writes (format = 2): its shares are project ids. A file with no format
// line is from a build that keyed shares by the project's slug (legacy):
// its shares are slugs, whatever they look like (a slug can be 26 digits,
// the shape of an id), and the daemon moves them to ids (hostkeys.go).
const HostFileFormat = 2

// ParseHostRunTargets reads a run_targets.toml; legacy: it has no format
// line (shares by slug). An empty file has none, and is legacy.
func ParseHostRunTargets(src string) (out []computeapi.HostRunTarget, legacy bool, err error) {
	doc, err := project.DecodeTOML(src)
	if err != nil {
		return nil, false, fmt.Errorf("run_targets.toml: %w", err)
	}
	r := project.NewTOMLReader("run_targets.toml", doc)
	_, hasFormat := doc["format"]
	if f := r.Int("format"); hasFormat && f != HostFileFormat {
		return nil, false, fmt.Errorf("run_targets.toml: format %d is not this build's (%d)", f, HostFileFormat)
	}
	legacy = !hasFormat
	for i, t := range r.Tables("run_targets") {
		where := fmt.Sprintf("run_targets.toml [[run_targets]] #%d", i+1)
		share := project.NewTOMLReader(where, t).Strs("share")
		rest := make(map[string]any, len(t))
		for k, v := range t {
			if k != "share" {
				rest[k] = v
			}
		}
		rt, err := parseRunTarget(where, rest)
		if err != nil {
			return nil, false, err
		}
		h := computeapi.HostRunTarget{RunTarget: rt, Share: share}
		if legacy {
			err = validateHostBox(h, api.ValidSlug, "a slug (in a file from a build that keyed shares by slug)")
		} else {
			err = ValidateHostRunTarget(h)
		}
		if err != nil {
			return nil, false, err
		}
		if slices.ContainsFunc(out, func(o computeapi.HostRunTarget) bool { return o.Name == h.Name }) {
			return nil, false, api.Errorf(api.CodeConflict, "run target %q defined twice", h.Name)
		}
		out = append(out, h)
	}
	if err := r.Done(); err != nil {
		return nil, false, err
	}
	return out, legacy, nil
}

// ValidateHostRunTarget is ValidateRunTarget for a machine-level binding: a
// box, with no repo (nothing to resolve a ref against), shared with at least
// one team project, by its id.
func ValidateHostRunTarget(h computeapi.HostRunTarget) error {
	return validateHostBox(h, ids.Valid, "a project id")
}

// validateHostBox is ValidateHostRunTarget with each share checked by valid
// (what is, a project id or an old file's slug).
func validateHostBox(h computeapi.HostRunTarget, valid func(string) bool, what string) error {
	if h.Kind != computeapi.RunTargetKindBox {
		return api.Errorf(api.CodeBadRequest, "run target %q: a machine-level binding is a box (kind box)", h.Name)
	}
	if h.Repo != "" || h.Branch != "" {
		return api.Errorf(api.CodeBadRequest, "run target %q: a machine-level box takes no repo or branch", h.Name)
	}
	if strings.Contains(h.Name, "@") { // runTargetName forbids it too; named for the clear error
		return api.Errorf(api.CodeBadRequest, "run target %q: a name has no @ (the authority adds @<machine>)", h.Name)
	}
	if err := ValidateRunTarget(nil, h.RunTarget); err != nil {
		return err
	}
	if _, ok := h.Config["share"]; ok {
		return api.Errorf(api.CodeBadRequest, "run target %q: config key share is zm's own", h.Name)
	}
	if len(h.Share) == 0 {
		return api.Errorf(api.CodeBadRequest, "run target %q: share names at least one team project", h.Name)
	}
	for _, p := range h.Share {
		if !valid(p) {
			return api.Errorf(api.CodeBadRequest, "run target %q: share %q is not %s", h.Name, p, what)
		}
	}
	return nil
}

// EncodeHostRunTargets writes a run_targets.toml.
func EncodeHostRunTargets(ts []computeapi.HostRunTarget) string {
	var b strings.Builder
	b.WriteString("# Machine-level box bindings (zm host share --pool, zm target bind --host).\n# Edited through the daemon; share lists the team projects that may use each.\n")
	fmt.Fprintf(&b, "format = %d\n", HostFileFormat)
	for _, t := range ts {
		encodeRunTarget(&b, t.RunTarget)
		fmt.Fprintf(&b, "share = %s\n", project.TOMLStrings(t.Share))
	}
	return b.String()
}

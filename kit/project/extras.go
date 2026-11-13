package project

import (
	"sync"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Extras is what a module adds to the registry's answers, and takes from
// its update request, for the fields of api.Project, api.AreaView and
// api.UpdateProjectRequest that are the module's own: usually the wire form
// of a project.toml key or section it owns (RegisterKey, RegisterSection).
// The registry knows nothing about those fields.
//
// Every func runs under the registry's lock and must not call the registry;
// f is the project's file, read-only (edit a copy: File.WithKey,
// File.WithSection). A nil func is skipped.
type Extras struct {
	// Project fills the module's fields of p, a project as sc may see it.
	// The registry has filled the rest (p.ID is the project; p.Areas are
	// the areas sc sees).
	Project func(p *api.Project, f File, sc authz.Scope)
	// Area fills the module's fields of v, one area as sc may see it
	// (v.Project.ID is the project, v.Area the area).
	Area func(v *api.AreaView, f File, sc authz.Scope)
	// Update applies the module's fields of req to f and returns the
	// result, which the registry validates (Section.Validate) and writes.
	// The caller is an admin of the project. owner: it is also the
	// machine's owner on its own socket, the only one who may change what
	// the daemon runs or reads on this machine; refuse such a change with
	// LocalSettingsError when it is false. It runs before the registry
	// applies its own fields of req, and only the result's Keys and
	// Sections are taken: a change to anything else is dropped.
	Update func(f File, req api.UpdateProjectRequest, owner bool) (File, error)
}

var (
	extrasMu sync.RWMutex
	extras   []Extras
)

// RegisterExtras adds x to what every registry asks (call it from an init).
func RegisterExtras(x Extras) {
	extrasMu.Lock()
	defer extrasMu.Unlock()
	extras = append(extras[:len(extras):len(extras)], x)
}

func extrasList() []Extras {
	extrasMu.RLock()
	defer extrasMu.RUnlock()
	return extras
}

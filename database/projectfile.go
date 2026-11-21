package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/protocol"
)

type Recipe struct{ Name, Image, Manifest string }
type section struct{}

func init() { project.RegisterSection("databases", section{}) }
func (section) Parse(where string, raw any) (any, error) {
	tables, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: databases must be [[databases]] tables", where)
	}
	var out []Recipe
	for _, table := range tables {
		r := project.NewTOMLReader(where+" [[databases]]", table)
		v := Recipe{r.Str("name"), r.Str("image"), r.Str("manifest")}
		if err := r.Done(); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (section) Validate(value any, f project.File) error {
	recipes, _ := value.([]Recipe)
	seen := map[string]bool{}
	for _, r := range recipes {
		_, err := Check(&protocol.Database{Project: f.ID, Name: r.Name, Image: r.Image, Manifest: r.Manifest})
		if err != nil {
			return err
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate database recipe %s", r.Name)
		}
		seen[r.Name] = true

	}
	return nil
}
func (section) Encode(value any) string {
	recipes, _ := value.([]Recipe)
	var b strings.Builder
	for _, r := range recipes {
		fmt.Fprintf(&b, "\n[[databases]]\nname = %s\nimage = %s\nmanifest = %s\n", project.TOMLString(r.Name), project.TOMLString(r.Image), project.TOMLString(r.Manifest))
	}
	return b.String()
}
func Resolve(reg *project.Registry, projectID, name string) (*protocol.Database, error) {
	for _, r := range Recipes(reg, projectID) {
		if r.Name == name {
			d := &protocol.Database{Project: projectID, Name: r.Name, Image: r.Image, Manifest: r.Manifest}
			_, err := Check(d)
			return d, err
		}
	}
	return nil, fmt.Errorf("project has no database recipe %q", name)
}

// Recipes is project projectID's database recipes, in project.toml order.
func Recipes(reg *project.Registry, projectID string) []Recipe {
	recipes, _ := reg.Section(projectID, "databases").([]Recipe)
	return slices.Clone(recipes)
}

// Put creates or replaces project projectID's recipe r.Name and rewrites
// project.toml, so the next Resolve answers it with no daemon restart. With
// ifImage set the recipe must exist with that image, else a conflict: a
// writer that read the old pin never overwrites a newer one. changed is
// false when the recipe was already r (whatever ifImage says); then nothing
// is written. The caller has checked who asked.
func Put(ctx context.Context, reg *project.Registry, projectID string, r Recipe, ifImage string) (changed bool, err error) {
	// The section's Validate runs Check again on the write; checked here
	// first so a refusal names its rule to the caller, not as an internal error.
	if _, err := Check(&protocol.Database{Project: projectID, Name: r.Name, Image: r.Image, Manifest: r.Manifest}); err != nil {
		return false, api.Errorf(api.CodeBadRequest, "%v", err)
	}
	return edit(ctx, reg, projectID, func(rs []Recipe) ([]Recipe, error) {
		i := slices.IndexFunc(rs, func(o Recipe) bool { return o.Name == r.Name })
		// Already r: a retry of a set whose answer was lost succeeds, whatever
		// pin it read before.
		if i >= 0 && rs[i] == r {
			return nil, errUnchanged
		}
		if ifImage != "" && (i < 0 || rs[i].Image != ifImage) {
			current := "no recipe"
			if i >= 0 {
				current = "image " + rs[i].Image
			}
			return nil, api.Errorf(api.CodeConflict, "database recipe %s has %s, not %s", r.Name, current, ifImage)
		}
		if i < 0 {
			return append(rs, r), nil
		}
		rs[i] = r
		return rs, nil
	})
}

// Remove deletes project projectID's recipe name and rewrites project.toml.
func Remove(ctx context.Context, reg *project.Registry, projectID, name string) error {
	_, err := edit(ctx, reg, projectID, func(rs []Recipe) ([]Recipe, error) {
		i := slices.IndexFunc(rs, func(o Recipe) bool { return o.Name == name })
		if i < 0 {
			return nil, api.NotFound("database recipe", name)
		}
		return slices.Delete(rs, i, i+1), nil
	})
	return err
}

// errUnchanged stops a write that would change nothing.
var errUnchanged = errors.New("database recipe unchanged")

// edit rewrites project id's recipes as fn says, under the registry's lock.
func edit(ctx context.Context, reg *project.Registry, id string, fn func([]Recipe) ([]Recipe, error)) (bool, error) {
	_, err := reg.SetSection(ctx, id, "databases", func(old any) (any, error) {
		rs, _ := old.([]Recipe)
		rs, err := fn(slices.Clone(rs)) // never the registry's own slice
		if err != nil || len(rs) == 0 {
			return nil, err
		}
		return rs, nil
	})
	if errors.Is(err, errUnchanged) {
		return false, nil
	}
	return err == nil, err
}

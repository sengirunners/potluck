package database

import (
	"fmt"
	"strings"

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
	recipes, _ := reg.Section(projectID, "databases").([]Recipe)
	for _, r := range recipes {
		if r.Name == name {
			d := &protocol.Database{Project: projectID, Name: r.Name, Image: r.Image, Manifest: r.Manifest}
			_, err := Check(d)
			return d, err
		}
	}
	return nil, fmt.Errorf("project has no database recipe %q", name)
}

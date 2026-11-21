package computeapi

// DatabaseRecipe is one of a project's database recipes ([[databases]] in
// project.toml): the name a sandbox create asks for with --database, the
// snapshot image pinned by digest, and that image's manifest (JSON text).
// None of it is secret.
type DatabaseRecipe struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Manifest string `json:"manifest"`
}

// DatabaseRecipes is GET /v0/databases: the project's recipes, in
// project.toml order.
type DatabaseRecipes struct {
	Project   string           `json:"project"`
	Databases []DatabaseRecipe `json:"databases"`
}

// SetDatabaseRecipeRequest is PUT /v0/databases/{name}: create or replace
// that recipe. IfImage, when set, is the image the current recipe must have
// (a recipe that differs, or none, is a conflict).
type SetDatabaseRecipeRequest struct {
	Image    string `json:"image"`
	Manifest string `json:"manifest"`
	IfImage  string `json:"if_image,omitempty"`
}

// DatabaseRecipeSet answers PUT /v0/databases/{name}. Changed is false when
// the recipe was already the one asked for: nothing was written.
type DatabaseRecipeSet struct {
	Project string         `json:"project"`
	Recipe  DatabaseRecipe `json:"recipe"`
	Changed bool           `json:"changed"`
}

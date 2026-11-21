package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/database"
	"github.com/spf13/cobra"
)

// sandboxDatabaseCmd lists, sets and removes the project's database recipes
// (the names sandbox create --database takes), live on the pool's daemon.
func (a *app) sandboxDatabaseCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "database", Short: "List the project's database recipes, the names --database takes",
		Long: `Lists the project's database recipes: each name sandbox create --database
takes and the snapshot image it boots. --json adds each recipe's manifest.
An owner or admin of the whole project sets or removes one with
database set and database rm; the very next sandbox create uses it, with no
daemon restart.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			l, err := computeclient.Databases(cmd.Context(), c)
			if err != nil {
				return err
			}
			return a.printDatabases(l)
		}}
	cmd.AddCommand(a.sandboxDatabaseSetCmd(), a.sandboxDatabaseRmCmd())
	return cmd
}

func (a *app) printDatabases(l computeapi.DatabaseRecipes) error {
	if a.asJSON() {
		return a.printJSON(l)
	}
	if len(l.Databases) == 0 {
		a.say("%s has no database recipes", l.Project)
	}
	for _, d := range l.Databases {
		a.say("%s %s", d.Name, d.Image)
	}
	return nil
}

func (a *app) sandboxDatabaseSetCmd() *cobra.Command {
	var req computeapi.SetDatabaseRecipeRequest
	var manifestFile string
	cmd := &cobra.Command{Use: "set <name> --image <name@sha256:digest> --manifest-file <path|->",
		Short: "Create or replace a database recipe (a project owner or admin); the next sandbox create uses it",
		Long: `Creates or replaces the project's database recipe <name>: the snapshot image,
pinned by digest, and that image's manifest, read byte for byte from
--manifest-file (- reads stdin). The pool's daemon checks the recipe as it
checks one in project.toml, writes project.toml, and the next sandbox create
--database <name> boots it; no daemon restart. Setting the recipe it already
has changes nothing. With --if-image the recipe must have that image now,
else nothing is written: a writer that read the old pin never overwrites a
newer one. An owner or admin of the whole project only.`,
		Example: "  " + a.cmd("sandbox database set app-db --image ghcr.io/acme/app-db@sha256:<64 hex> --manifest-file manifest.json"),
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitWith{2, errors.New("database set takes one recipe name")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := readManifest(manifestFile, a.host.Stdin)
			if err != nil {
				return exitWith{2, err}
			}
			req.Manifest = m
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			v, err := computeclient.SetDatabase(cmd.Context(), c, args[0], req)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(v)
			}
			if !v.Changed {
				a.say("%s already boots %s; nothing changed", v.Recipe.Name, v.Recipe.Image)
				return nil
			}
			a.say("%s now boots %s", v.Recipe.Name, v.Recipe.Image)
			return nil
		}}
	cmd.Flags().StringVar(&req.Image, "image", "", "the snapshot image, pinned: <name>@sha256:<64 hex>")
	cmd.Flags().StringVar(&manifestFile, "manifest-file", "", "the image's manifest JSON, byte for byte; - reads stdin")
	cmd.Flags().StringVar(&req.IfImage, "if-image", "", "set only if the recipe's image is this one now")
	_ = cmd.MarkFlagRequired("image")
	_ = cmd.MarkFlagRequired("manifest-file")
	return cmd
}

func (a *app) sandboxDatabaseRmCmd() *cobra.Command {
	return &cobra.Command{Use: "rm <name>", Short: "Remove a database recipe (a project owner or admin)",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitWith{2, errors.New("database rm takes one recipe name")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.inPool(cmd.Context())
			if err != nil {
				return err
			}
			l, err := computeclient.RemoveDatabase(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if !a.asJSON() {
				a.say("removed %s", args[0])
			}
			return a.printDatabases(l)
		}}
}

// readManifest reads a recipe's manifest from path (- is stdin) as it is:
// the image packages these exact bytes.
func readManifest(path string, stdin io.Reader) (string, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("--manifest-file: %v", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, database.ManifestMax+1))
	switch {
	case err != nil:
		return "", fmt.Errorf("--manifest-file: %v", err)
	case len(b) > database.ManifestMax:
		return "", fmt.Errorf("--manifest-file: over %d KiB", database.ManifestMax>>10)
	}
	return string(b), nil
}

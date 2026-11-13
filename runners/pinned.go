package runners

// Pinned code for job grants (docs/daemon.md "Job grants"): a grant naming
// a workflow file trusts that file at the run's commit, and so everything
// it pulls in with uses:. A tag or branch an action is named by can move
// after review; a full commit id cannot. So a run gets its default
// branch's ref (and with it a grant) only when every uses: in its workflow
// file, and in every local workflow or action that file pulls in, read at
// the run's own commit, names a full commit id (never another repository's
// reusable workflow, whose own uses: are not read), a docker image by
// digest, or a local path, and every image the job or an action runs
// (container, services, runs.image) is named by digest. What a pinned action pulls in at run time is not read
// here (docs/daemon.md, "Preconditions the door cannot check").

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/sengirunners/potluck/ghapp"
	"gopkg.in/yaml.v3"
)

// fileReader reads one file of a repository at a commit (ghapp.Client).
type fileReader interface {
	FileAt(ctx context.Context, installation int64, repo, path, sha string) ([]byte, error)
}

var (
	pinnedActionRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[^@\s]+)?@[0-9a-f]{40}$`)
	pinnedDockerRE = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)
	pinnedImageRE  = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
)

// pinnedFiles bounds how many files one run's check reads.
const pinnedFiles = 32

// unpinnedUses is why the workflow file at workflow in repo at commit sha
// may run code that can move ("" when it cannot). err is GitHub not
// answering; a file that is not there or not YAML is a why.
func unpinnedUses(ctx context.Context, cl fileReader, installation int64, repo, workflow, sha string) (string, error) {
	type file struct {
		path   string
		action bool // an action directory: action.yml, else action.yaml
	}
	queue := []file{{path: workflow}}
	seen := map[string]bool{workflow: true}
	for len(queue) > 0 {
		if len(seen) > pinnedFiles {
			return fmt.Sprintf("it pulls in more than %d local workflows and actions", pinnedFiles), nil
		}
		f := queue[0]
		queue = queue[1:]
		name, body, err := readUses(ctx, cl, installation, repo, f.path, f.action, sha)
		if err != nil || name == "" {
			return fmt.Sprintf("%s could not be read at its commit", f.path), err
		}
		doc, why := oneDoc(body)
		if why != "" {
			return name + " " + why, nil
		}
		uses, images, why := usesOf(doc)
		if why != "" {
			return name + ": " + why, nil
		}
		for _, im := range images {
			if !pinnedImageRE.MatchString(strings.TrimPrefix(im, "docker://")) {
				return fmt.Sprintf("%s: image %q is not pinned to a digest", name, im), nil
			}
		}
		for _, u := range uses {
			local, isLocal := strings.CutPrefix(u, "./")
			switch {
			case strings.Contains(u, "${{"):
				return fmt.Sprintf("%s: uses %q is an expression", name, u), nil
			case isLocal:
				p := path.Clean(local)
				if p == "." || strings.HasPrefix(p, "../") || p == ".." || strings.HasPrefix(p, "/") {
					return fmt.Sprintf("%s: uses %q leaves the repository", name, u), nil
				}
				next := file{path: p, action: !(strings.HasPrefix(p, ".github/workflows/") && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")))}
				if !seen[next.path] {
					seen[next.path] = true
					queue = append(queue, next)
				}
			case strings.HasPrefix(u, "docker://"):
				if !pinnedDockerRE.MatchString(u) {
					return fmt.Sprintf("%s: uses %q is not pinned to an image digest", name, u), nil
				}
			case !pinnedActionRE.MatchString(u):
				return fmt.Sprintf("%s: uses %q is not pinned to a full commit id", name, u), nil
			case strings.Contains(u, "/.github/workflows/"):
				// Its own uses: run in this job too and are not read here.
				return fmt.Sprintf("%s: uses %q is another repository's reusable workflow", name, u), nil
			}
		}
	}
	return "", nil
}

// readUses reads one file the check walks: a workflow file, or an action
// directory's action.yml (else action.yaml). name is the file read, ""
// when there is none.
func readUses(ctx context.Context, cl fileReader, installation int64, repo, p string, action bool, sha string) (string, []byte, error) {
	if !action {
		b, err := cl.FileAt(ctx, installation, repo, p, sha)
		if ghapp.NotFound(err) {
			return "", nil, nil
		}
		return p, b, err
	}
	for _, n := range []string{p + "/action.yml", p + "/action.yaml"} {
		b, err := cl.FileAt(ctx, installation, repo, n, sha)
		if ghapp.NotFound(err) {
			continue
		}
		return n, b, err
	}
	return "", nil, nil
}

// oneDoc is body's one YAML document, or why not: a file of several
// documents is refused (GitHub reads one; this check reads all it trusts).
func oneDoc(body []byte) (*yaml.Node, string) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var doc, more yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, "is not YAML this check can read"
	}
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, "is not one YAML document"
	}
	return &doc, ""
}

// usesOf is every value of a uses: key anywhere in doc (a job's reusable
// workflow, a step's action, and any other mapping's: reading more than
// GitHub would only refuses more), and every image a job or action runs:
// a job's container (or its image:), each of its services' image:, and an
// action's runs.image (a Dockerfile, built from whatever it names, is not
// an image by digest, so it is refused). Keys are matched in any letter
// case. A uses:, container: or image: whose value is not one plain string
// (where one is meant) is a why.
func usesOf(doc *yaml.Node) (uses, images []string, why string) {
	scalar := func(n *yaml.Node) (string, bool) {
		return strings.TrimSpace(n.Value), n.Kind == yaml.ScalarNode
	}
	imageOf := func(n *yaml.Node) string { // a mapping's image:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if strings.EqualFold(n.Content[i].Value, "image") {
				v, ok := scalar(n.Content[i+1])
				if !ok {
					return "an image: is not one string"
				}
				images = append(images, v)
			}
		}
		return ""
	}
	var walk func(n *yaml.Node) string
	walk = func(n *yaml.Node) string {
		if n.Kind == yaml.AliasNode {
			return "it uses a YAML alias"
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Kind != yaml.ScalarNode {
					continue
				}
				switch strings.ToLower(k.Value) {
				case "uses":
					s, ok := scalar(v)
					if !ok {
						return "a uses: is not one string"
					}
					uses = append(uses, s)
				case "container", "runs":
					if s, ok := scalar(v); ok && strings.EqualFold(k.Value, "container") && s != "" {
						images = append(images, s)
					} else if v.Kind == yaml.MappingNode {
						if why := imageOf(v); why != "" {
							return why
						}
					}
				case "services":
					if v.Kind == yaml.MappingNode {
						for j := 1; j < len(v.Content); j += 2 {
							if v.Content[j].Kind != yaml.MappingNode {
								return "a service is not a mapping"
							}
							if why := imageOf(v.Content[j]); why != "" {
								return why
							}
						}
					}
				}
			}
		}
		for _, c := range n.Content {
			if why := walk(c); why != "" {
				return why
			}
		}
		return ""
	}
	why = walk(doc)
	return uses, images, why
}

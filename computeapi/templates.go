package computeapi

import "sort"

// boxTemplates are zm's box templates (zm-eakh.12.5.20), each the name of the
// image it boots: acquire's template picks the binding's image of that name,
// any digest (a binding lent by another zm pins another build of it).
// hostprobe's images.pin pins each beside its layer's default image,
// and its test keeps the two agreeing.
var boxTemplates = map[string]string{
	"browser": "ghcr.io/sengirunners/potluck-box-browser", // sandboxed Chromium, Playwright and PNG screenshots
	"agent":   "ghcr.io/sengirunners/potluck-box-agent",   // git, gh, Node, Go, Python, the Claude Code and Codex CLIs
}

// BoxTemplateImage is the image name template t boots.
func BoxTemplateImage(t string) (string, bool) {
	im, ok := boxTemplates[t]
	return im, ok
}

// BoxTemplates are zm's box template names, sorted.
func BoxTemplates() []string {
	out := make([]string, 0, len(boxTemplates))
	for t := range boxTemplates {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

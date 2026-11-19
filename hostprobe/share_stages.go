package hostprobe

import (
	"fmt"
	"strings"
)

// preflightWords is the preflight stage's result once every check before a
// download passed: the layer, and what comes next.
func preflightWords(res ShareResult) string {
	return fmt.Sprintf("layer %s ready; %s and %s to check next", res.Layer, plural(len(res.Pulls), "image"), plural(len(res.Sizes), "size"))
}

// imagesWords is the images stage's result: how many are ready, how many
// of those were here already.
func imagesWords(ready, reused, of int) string {
	s := fmt.Sprintf("%d of %d ready", ready, of)
	var how []string
	if n := ready - reused; n > 0 {
		how = append(how, fmt.Sprintf("%d pulled", n))
	}
	if reused > 0 {
		how = append(how, fmt.Sprintf("%d reused", reused))
	}
	if len(how) > 0 {
		s += " (" + strings.Join(how, ", ") + ")"
	}
	if ready < of {
		s += "; the note below says which did not pull"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

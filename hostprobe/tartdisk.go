package hostprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/imagepins"
)

// tartImageBudgetGB is the free disk, in GB, one image needs where Tart
// stores it: its pin's min_free_gb. An image with no pin of its own takes the
// budget of its repository's pin, else of the default image, and estimated is
// true: that is a guess, never silence.
func tartImageBudgetGB(image string) (gb int, estimated bool, from string) {
	if p, ok := PinForImage(image); ok {
		return p.MinFreeGB, false, ""
	}
	if p, ok := imagepins.PinForRepository(imageRepository(image) + "@"); ok {
		return p.MinFreeGB, true, "the budget of the pin for its repository"
	}
	return tartDefaultFreeGB(), true, "the budget of Potluck's default image"
}

// imageRepository is an image reference without its digest or tag.
func imageRepository(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo
}

// DiskNeedGB is the free disk in GB the images need on a Mac (the largest
// budget among them: tartImageBudgetGB), and one line for the
// images whose budget is an estimate ("" when every image has a pin).
func DiskNeedGB(images []string) (need int, estimate string) {
	var guesses []string
	for _, im := range images {
		gb, estimated, from := tartImageBudgetGB(im)
		need = max(need, gb)
		if estimated {
			name, _, _ := strings.Cut(im, "@")
			guesses = append(guesses, fmt.Sprintf("%s (%d GB, %s)", name, gb, from))
		}
	}
	if len(guesses) > 0 {
		estimate = "the disk budget is an estimate for an image with no pin: " + strings.Join(guesses, "; ")
	}
	return need, estimate
}

// tartDiskCheck is share's one disk rule for a Mac, run before any pull (in a
// dry run too). It refuses when the free disk where Tart stores images is
// under what the images still to be pulled need; an image Tart already has
// needs no pull, so it counts for nothing, and a Mac whose images are all
// present is never refused. A disk that could not be read is not refused,
// but note says the check was skipped. Tart's image list is read once, and
// only when the disk is short; when it cannot be read (or run is nil), every
// image counts as still to be pulled and the refusal says so. note also says
// when an image's budget is an estimate.
func tartDiskCheck(ctx context.Context, run Runner, r Report, images []string) (note string, err error) {
	all, estimate := DiskNeedGB(images)
	if r.TartDiskUnread != "" {
		return fmt.Sprintf("the disk check was skipped: could not read the free disk in %s, where Tart stores images (%s); make sure about %d GB is free there before lending",
			r.TartDir, r.TartDiskUnread, all) + estimateSuffix(estimate), nil
	}
	if r.TartDir == "" && r.TartFreeGB == 0 { // never measured
		return "", nil
	}
	if r.TartFreeGB >= float64(all) {
		return estimate, nil
	}
	cached, listErr := tartCachedImages(ctx, run)
	var toPull []string
	for _, im := range images {
		if !cached[im] {
			toPull = append(toPull, im)
		}
	}
	need, _ := DiskNeedGB(toPull)
	if r.TartFreeGB >= float64(need) {
		return estimate, nil
	}
	names := make([]string, len(toPull))
	for i, im := range toPull {
		names[i], _, _ = strings.Cut(im, "@")
	}
	why := ""
	if listErr != nil {
		why = fmt.Sprintf(" (Tart's image list could not be read: %v; so every image counts as still to pull)", listErr)
	}
	return "", fmt.Errorf("not enough disk to lend %s: only %.0f GB free in %s, where Tart stores images, and the pull, the unpacked image and its boxes need at least %d GB%s. Free some space, or move Tart's images to a bigger disk: set TART_HOME there in the shell that runs this, and in the environment of the potluck daemon too (a daemon started from another shell still looks in ~/.tart: stop it with `potluck daemon stop`, and run this again from the shell with TART_HOME set); nothing was downloaded%s",
		strings.Join(names, ", "), r.TartFreeGB, r.TartDir, need, why, estimateSuffix(estimate))
}

func estimateSuffix(estimate string) string {
	if estimate == "" {
		return ""
	}
	return "; " + estimate
}

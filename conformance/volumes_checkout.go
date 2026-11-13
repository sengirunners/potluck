package conformance

import (
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/protocol"
)

// A box's checkout in a volume (capability volumes-seal, zm-eakh.12.65.6):
// a cow volume with checkout true is where a box asked for a commit gets
// its checkout, so code sits beside what the volume holds (a deps layer's
// node_modules) at the paths it was built at. The checkout writes the
// commit's files over the box's view and leaves every other file there;
// like any cow write, none of it reaches the volume.
func (r *run) checkouts(owner, name, planted string) {
	r.add("checkout with rw, ro or build, or on two volumes, answers bad_request", r.checkoutBadArgs(owner, name))
	const head = "a box's checkout goes into its cow volume marked checkout, beside the volume's files, never into the volume"
	if r.opt.Ref == "" || r.opt.Commit == "" {
		r.skip(head, "no --ref and --commit: a box's checkout is optional")
		return
	}
	r.add(head, r.checkoutInVolume(owner, name))
	r.add("a .git a checkout volume holds never changes the box's checkout (its config, hooks)", r.checkoutPlantedGit(owner, planted))
}

// checkoutUnknown: a driver without volumes-seal refuses checkout, never
// putting the checkout in its own place as if it had not been asked.
func (r *run) checkoutUnknown(owner, name string) error {
	res, e, err := r.acquire(args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner,
		"volumes", []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeCOW, "checkout": true}}), r.cfg)
	if e == nil && err == nil {
		r.end(res.Handle)
	}
	return wantCode(e, err, protocol.CodeBadRequest)
}

func (r *run) checkoutBadArgs(owner, name string) error {
	v := func(n, p, mode string) map[string]any {
		return map[string]any{"name": n, "path": p, "mode": mode, "checkout": true}
	}
	for _, c := range []struct {
		why  string
		vols []map[string]any
	}{
		{"checkout with rw", []map[string]any{v(name, volPath, protocol.VolumeRW)}},
		{"checkout with ro", []map[string]any{v(name, volPath, protocol.VolumeRO)}},
		{"checkout with build", []map[string]any{v(name, volPath, protocol.VolumeBuild)}},
		{"checkout on two volumes", []map[string]any{v(name, volPath, protocol.VolumeCOW), v(name+"-2", volPath+"-2", protocol.VolumeCOW)}},
	} {
		res, e, err := r.acquire(args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "volumes", c.vols), r.cfg)
		if e == nil && err == nil {
			r.end(res.Handle)
		}
		if err := wantCode(e, err, protocol.CodeBadRequest); err != nil {
			return fmt.Errorf("%s: %v", c.why, err)
		}
	}
	return nil
}

// volLayer is what the check puts in the volume before the checkout box:
// node_modules/zmconf/f, and a probe.sh that the commit's must replace.
const volLayer = `cd "$HOME/$1" && mkdir -p node_modules/zmconf && printf '%s' "$2" > node_modules/zmconf/f && printf layer > probe.sh`

func (r *run) checkoutInVolume(owner, name string) error {
	f, e, err := r.acquireVol(owner, name, protocol.VolumeRW)
	if err = must(e, err); err == nil {
		var res execResult
		if res, err = r.exec(f.Handle, []string{"sh", "-c", volLayer, "sh", volPath, r.tag}, "", 0); err == nil && *res.Exit != 0 {
			err = fmt.Errorf("filling the volume exited %d: %s", *res.Exit, clip(*res.Stderr))
		}
	}
	r.endHeld(f)
	if err != nil {
		return fmt.Errorf("the volume's files: %v", err)
	}

	sent := args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "ref", r.opt.Ref, "commit", r.opt.Commit,
		"volumes", []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeCOW, "checkout": true}})
	a, e, err := r.acquire(sent, r.cfg)
	b := held{acquired: a, sent: sent}
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return err
	}
	if len(a.Volumes) != 1 || !a.Volumes[0].Checkout {
		return fmt.Errorf("acquire answered volumes %+v, want the one asked with checkout true", a.Volumes)
	}
	// exec's default cwd is the checkout; it is the volume's path, at the commit,
	// with the volume's node_modules and the commit's probe.sh.
	look := `here=$(pwd -P); vol=$(cd "$HOME/$1" && pwd -P) || exit 3
[ "$here" = "$vol" ] || { echo "cwd $here, not the volume at $vol"; exit 0; }
printf '%s|%s|%s' "$(git rev-parse HEAD)" "$(cat node_modules/zmconf/f 2>/dev/null)" "$(cat probe.sh)"`
	res, err := r.sh(a.Handle, look, volPath)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("looking at the checkout exited %d: %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return err
	}
	got := strings.SplitN(*res.Stdout, "|", 3)
	switch {
	case len(got) != 3:
		return fmt.Errorf("the checkout box's default cwd: %s", clip(*res.Stdout))
	case got[0] != r.opt.Commit:
		return fmt.Errorf("HEAD in the volume is %q, sent %q", got[0], r.opt.Commit)
	case got[1] != r.tag:
		return fmt.Errorf("the checkout took the volume's node_modules/zmconf/f: it reads %q, want %q", clip(got[1]), r.tag)
	case got[2] == "layer":
		return fmt.Errorf("probe.sh is the volume's, not the commit's: the commit's files must replace what is there")
	}
	if res, err = r.sh(a.Handle, `printf changed > node_modules/zmconf/f`, ""); err == nil && *res.Exit != 0 {
		err = fmt.Errorf("a write in the checkout exited %d", *res.Exit)
	}
	if err != nil {
		return err
	}
	r.endHeld(b)

	// The volume is as it was: no checkout, no write of the box's.
	ro, e, err := r.acquireVol(owner, name, protocol.VolumeRO)
	defer r.endHeld(ro)
	if err = must(e, err); err != nil {
		return err
	}
	res, err = r.sh(ro.Handle, `cd "$HOME/$1" || exit 3; printf '%s|%s|' "$(cat node_modules/zmconf/f)" "$(cat probe.sh)"; [ -e .git ] && printf git; true`, volPath)
	if err == nil && *res.Stdout != r.tag+"|layer|" {
		err = fmt.Errorf("after a checkout box ended the volume reads %q, want %q (its own files, no .git)", clip(*res.Stdout), r.tag+"|layer|")
	}
	return err
}

// gitPlant is a .git in the volume whose config would change a checkout
// that trusted it: a hooks path in the volume whose post-checkout hook
// rewrites probe.sh (a relative hooks path is the worktree's).
const gitPlant = `set -e; cd "$HOME/$1"; printf layer > probe.sh
git init -q .; git config core.hooksPath .git/zmhooks; mkdir -p .git/zmhooks
printf '#!/bin/sh\nprintf planted > probe.sh\n' > .git/zmhooks/post-checkout; chmod +x .git/zmhooks/post-checkout`

func (r *run) checkoutPlantedGit(owner, name string) error {
	f, e, err := r.acquireVol(owner, name, protocol.VolumeRW)
	if err = must(e, err); err == nil {
		var res execResult
		if res, err = r.sh(f.Handle, gitPlant, volPath); err == nil && *res.Exit != 0 {
			err = fmt.Errorf("planting a .git exited %d: %s", *res.Exit, clip(*res.Stderr))
		}
	}
	r.endHeld(f)
	if err != nil {
		return fmt.Errorf("the volume's files: %v", err)
	}
	sent := args("request_id", r.key(), "actor", actor, "ttl_s", r.longTTL(), "volume_owner", owner, "ref", r.opt.Ref, "commit", r.opt.Commit,
		"volumes", []map[string]any{{"name": name, "path": volPath, "mode": protocol.VolumeCOW, "checkout": true}})
	a, e, err := r.acquire(sent, r.cfg)
	b := held{acquired: a, sent: sent}
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return err
	}
	look := `cd "$HOME/$1" || exit 3
git rev-parse HEAD; case $(cat probe.sh) in planted) echo planted ;; layer) echo layer ;; *) echo commit ;; esac
git config --get core.hooksPath; true`
	res, err := r.sh(a.Handle, look, volPath)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("looking at the checkout exited %d: %s", *res.Exit, clip(*res.Stderr))
	}
	if err != nil {
		return err
	}
	got := strings.SplitN(strings.TrimSuffix(*res.Stdout, "\n"), "\n", 3)
	switch {
	case len(got) < 2:
		return fmt.Errorf("the checkout: %s", clip(*res.Stdout))
	case got[0] != r.opt.Commit:
		return fmt.Errorf("HEAD in the volume is %q, sent %q", got[0], r.opt.Commit)
	case got[1] == "planted":
		return fmt.Errorf("the volume's .git ran its post-checkout hook: the checkout trusted a .git it did not make")
	case got[1] == "layer":
		return fmt.Errorf("probe.sh is the volume's, not the commit's")
	case len(got) == 3:
		return fmt.Errorf("the checkout's git config has the volume's core.hooksPath %q: the volume's .git was kept", clip(got[2]))
	}
	return nil
}

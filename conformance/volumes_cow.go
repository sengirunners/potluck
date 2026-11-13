package conformance

import "fmt"

// A cow or ro view is the box's own (zm-eakh.12.65.2): nothing a box writes
// through it reaches a sibling, a later box or the volume, and the view is
// the volume's tree as it is (a symlink stays a symlink, never the bytes it
// names). Runs after "a cow box's writes are its own", with the volume
// holding f = r.tag and no box holding it.
func (r *run) cowViews(owner, name string) {
	r.add("cow siblings do not see each other's writes (two live cow boxes of one owner)", r.cowSiblings(owner, name))
	r.add("a plain box after a cow box (and a ro box) sees nothing of them", r.cowPlainAfter(owner, name))
	r.add("a later cow box sees none of an ended cow box's writes", r.cowLater(owner, name, "cow"))
	r.add("a later ro box sees none of an ended cow box's writes", r.cowLater(owner, name, "ro"))
	r.add("a symlink in a volume stays a symlink in a ro or cow view", r.volLinks(owner, name))
}

// cowMark writes mark through box h's view: f becomes mark, and new files
// only-<mark> and d-<mark>/g appear.
func (r *run) cowMark(h, mark string) error {
	res, err := r.exec(h, []string{"sh", "-c", `cd "$HOME/$1" && printf '%s' "$2" > f && printf x > "only-$2" && mkdir "d-$2" && printf x > "d-$2/g"`,
		"sh", volPath, mark}, "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("a write through a cow view exited %d: %s", *res.Exit, clip(*res.Stderr))
	}
	return err
}

// cowView is what box h's view holds: f's bytes, then every other name in it.
func (r *run) cowView(h string) (string, error) {
	res, err := r.sh(h, `cd "$HOME/$1" || exit 1; printf 'f=%s' "$(cat f)"; for x in * .[!.]* ..?*; do [ "$x" != f ] && { [ -e "$x" ] || [ -L "$x" ]; } && printf ' %s' "$x"; done; true`, volPath)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("the view is not there: exit %d", *res.Exit)
	}
	if err != nil {
		return "", err
	}
	return *res.Stdout, nil
}

func (r *run) cowSiblings(owner, name string) error {
	a, e, err := r.acquireVol(owner, name, "cow")
	defer r.endHeld(a)
	if err = must(e, err); err != nil {
		return err
	}
	b, e, err := r.acquireVol(owner, name, "cow")
	defer r.endHeld(b)
	if err = must(e, err); err != nil {
		return err
	}
	ma, mb := "cow-a-"+r.tag, "cow-b-"+r.tag
	steps := []struct{ write, read, mark, want string }{
		{a.Handle, b.Handle, ma, "f=" + r.tag},
		{b.Handle, a.Handle, mb, "f=" + ma + " d-" + ma + " only-" + ma},
	}
	for _, s := range steps {
		if err := r.cowMark(s.write, s.mark); err != nil {
			return err
		}
		got, err := r.cowView(s.read)
		if err != nil {
			return err
		}
		if got != s.want {
			return fmt.Errorf("after a sibling wrote %s, a cow box's view holds %q, want %q", s.mark, clip(got), s.want)
		}
	}
	return nil
}

// cowPlainAfter: a cow box writes and ends beside a ro box; a box with no
// volumes after them has nothing at the mount path and no marker.
func (r *run) cowPlainAfter(owner, name string) error {
	c, e, err := r.acquireVol(owner, name, "cow")
	defer r.endHeld(c)
	if err = must(e, err); err != nil {
		return err
	}
	ro, e, err := r.acquireVol(owner, name, "ro")
	defer r.endHeld(ro)
	if err = must(e, err); err != nil {
		return err
	}
	mark := "cow-c-" + r.tag
	if err := r.cowMark(c.Handle, mark); err != nil {
		return err
	}
	r.endHeld(c)
	r.endHeld(ro)
	p, err := r.acquireAny(r.longTTL())
	defer r.endHeld(p)
	if err != nil {
		return err
	}
	return r.nothingAt(p.Handle, mark)
}

// cowLater: a cow box writes and ends; a box after it with a mode view
// (cow or ro) sees the volume as it is (f = r.tag and nothing else).
func (r *run) cowLater(owner, name, mode string) error {
	c, e, err := r.acquireVol(owner, name, "cow")
	defer r.endHeld(c)
	if err = must(e, err); err != nil {
		return err
	}
	mark := "cow-later-" + mode + "-" + r.tag
	if err := r.cowMark(c.Handle, mark); err != nil {
		return err
	}
	r.endHeld(c)
	v, e, err := r.acquireVol(owner, name, mode)
	defer r.endHeld(v)
	if err = must(e, err); err != nil {
		return err
	}
	got, err := r.cowView(v.Handle)
	if err == nil && got != "f="+r.tag {
		err = fmt.Errorf("after a cow box wrote %s and ended, a %s view holds %q, want %q", mark, mode, clip(got), "f="+r.tag)
	}
	return err
}

func (r *run) volLinks(owner, name string) error {
	w, e, err := r.acquireVol(owner, name, "rw")
	defer r.endHeld(w)
	if err = must(e, err); err != nil {
		return err
	}
	// l names a file in the volume, n names nothing, e names a host file
	// that is there on any host: none may become the bytes it names.
	nowhere := "/zmconf-nowhere-" + r.tag
	res, err := r.exec(w.Handle, []string{"sh", "-c", `cd "$HOME/$1" && rm -f l n e && ln -s f l && ln -s "$2" n && ln -s /etc/hosts e`, "sh", volPath, nowhere}, "", 0)
	if err == nil && *res.Exit != 0 {
		err = fmt.Errorf("a rw box could not make symlinks in its volume: exit %d %s", *res.Exit, clip(*res.Stderr))
	}
	r.endHeld(w)
	if err != nil {
		return err
	}
	want := "f\n" + nowhere + "\n/etc/hosts\n"
	for _, mode := range []string{"ro", "cow"} {
		v, e, err := r.acquireVol(owner, name, mode)
		if err = must(e, err); err == nil {
			var res execResult
			if res, err = r.sh(v.Handle, `cd "$HOME/$1" && [ -L l ] && [ -L n ] && [ -L e ] && readlink l && readlink n && readlink e`, volPath); err == nil && *res.Stdout != want {
				err = fmt.Errorf("in a %s view the volume's symlinks read %q, want %q (links, not what they name)", mode, clip(*res.Stdout), want)
			}
		}
		r.endHeld(v)
		if err != nil {
			return fmt.Errorf("%s: %w", mode, err)
		}
	}
	return nil
}

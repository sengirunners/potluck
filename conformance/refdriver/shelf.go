package refdriver

import (
	"crypto/sha512"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/sengirunners/potluck/protocol"
)

// The package shelf (docs/protocol.md, The package shelf; zm-eakh.12.30):
// config shelf=1 keeps a host directory (state/shelf) as the shelf and shows
// it in each box at HOME/potluck-shelf (describe.shelf.path, relative to
// HOME: a box here is a directory, with no root of its own). The box's view
// is a fresh copy with no write bits, made again before each exec, so
// nothing a box does to it (a chmod and a write in one exec included)
// outlasts that exec or reaches the shelf or another box. The box runs as
// this machine's user, who owns the copy: a chmod there works, which a
// read-only mount refuses.

// shelfView is where a box sees the shelf, relative to its HOME.
const shelfView = "potluck-shelf"

func (d *drv) shelfOn() bool {
	return d.cfg["kind"] == "box" && d.cfg["shelf"] == "1"
}

func shelfMaster(state string) string { return filepath.Join(state, "shelf") }

// shelfDescribe adds capability shelf and describe.shelf.
func (d *drv) shelfDescribe(out map[string]any, caps []string) []string {
	if !d.shelfOn() {
		return caps
	}
	out["shelf"] = map[string]any{"path": shelfView}
	if d.is("shelf-no-path") {
		out["shelf"] = map[string]any{}
	}
	return append(caps, protocol.CapShelf)
}

// shelfBlob is the one npm tarball the reference shelf holds.
var shelfBlob = []byte("refdriver shelf tarball\n")

// ensureShelf makes the shelf once: the helper and one blob, files 0444. A
// copy built aside and renamed in, as several driver processes may race.
func ensureShelf(state string) error {
	dst := shelfMaster(state)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	tmp, err := os.MkdirTemp(state, "shelf-new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	sum := sha512.Sum512(shelfBlob)
	hx := hex.EncodeToString(sum[:])
	files := map[string][]byte{
		"bin/potluck-deps": []byte("#!/bin/sh\nexit 0\n"),
		filepath.Join("npm/content-v2/sha512", hx[:2], hx[2:4], hx[4:]): shelfBlob,
	}
	for rel, b := range files {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o444); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		if _, serr := os.Stat(dst); serr == nil {
			return nil // another process made it
		}
		return err
	}
	return nil
}

// showShelf puts box r's view of the shelf in place before a command.
func (d *drv) showShelf(r record) error {
	if !d.shelfOn() || r.Dir == "" {
		return nil
	}
	state := filepath.Dir(r.Dir)
	if err := ensureShelf(state); err != nil {
		return err
	}
	dst := filepath.Join(r.Dir, shelfView)
	switch {
	case d.is("shelf-writable"): // the box's own copy, writable, kept
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
		return exec.Command("cp", "-R", shelfMaster(state), dst).Run()
	case d.is("shelf-shared-copy"): // one writable copy every box sees
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
		shared := filepath.Join(state, "shelf-shared")
		if _, err := os.Stat(shared); err != nil {
			if err := exec.Command("cp", "-R", shelfMaster(state), shared).Run(); err != nil {
				return err
			}
		}
		return os.Symlink(shared, dst)
	case d.is("shelf-sticky-link"): // a link to one shared directory any box may add to (1777), its files read-only
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
		shared := filepath.Join(state, "shelf-sticky")
		if _, err := os.Stat(shared); err != nil {
			if err := exec.Command("cp", "-R", shelfMaster(state), shared).Run(); err != nil {
				return err
			}
			if err := exec.Command("chmod", "-R", "a-w", shared).Run(); err != nil {
				return err
			}
			if err := os.Chmod(shared, 0o1777); err != nil {
				return err
			}
		}
		return os.Symlink(shared, dst)
	}
	unlockTree(dst)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	tmp := dst + ".new-" + strconv.Itoa(os.Getpid())
	_ = os.RemoveAll(tmp)
	if err := exec.Command("cp", "-R", shelfMaster(state), tmp).Run(); err != nil {
		return err
	}
	if err := exec.Command("chmod", "-R", "a-w", tmp).Run(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// boxCache is what a box leaves in its HOME by using the shelf: its npm
// cache and its wants.
var boxCache = []string{".npm", filepath.Dir(protocol.ShelfWants)}

// endShelf is what the shelf bugs do with an ending box (the lock is held):
// copy its npm cache's blobs onto the shelf (shelf-harvests-box), or keep
// its cache and wants for the next box (shelf-cache-kept) or the next warm
// box (warm-shelf-cache-kept).
func endShelf(r record, bug string) {
	state := filepath.Dir(r.Dir)
	switch bug {
	case "shelf-harvests-box":
		src := filepath.Join(r.Dir, ".npm/_cacache/content-v2/sha512")
		_ = filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(src, p)
			dst := filepath.Join(shelfMaster(state), "npm/content-v2/sha512", rel)
			if b, err := os.ReadFile(p); err == nil && os.MkdirAll(filepath.Dir(dst), 0o755) == nil {
				_ = os.WriteFile(dst, b, 0o444)
			}
			return nil
		})
	case "shelf-cache-kept":
		moveCache(r.Dir, filepath.Join(state, "shelf-kept"))
	case "warm-shelf-cache-kept":
		for _, w := range records(state) {
			if w.State == "warm" {
				moveCache(r.Dir, w.Dir)
				return
			}
		}
	}
}

// moveCache moves boxCache from one directory to another.
func moveCache(from, to string) {
	_ = os.MkdirAll(to, 0o700)
	for _, n := range boxCache {
		_ = os.RemoveAll(filepath.Join(to, n))
		_ = os.Rename(filepath.Join(from, n), filepath.Join(to, n))
	}
}

// startShelf is shelf-cache-kept's other half: a new box gets what the
// last box kept.
func (d *drv) startShelf(r record) {
	if d.is("shelf-cache-kept") {
		moveCache(filepath.Join(filepath.Dir(r.Dir), "shelf-kept"), r.Dir)
	}
}

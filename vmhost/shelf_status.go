package vmhost

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// shelfStatusCmd is `potluck-driver-tart host shelf status`: what describe
// lists of the shelf (its path, bytes and files; no names), the fill mode,
// the limits and the size of the queue of boxes' wants. It reads; it never
// creates the volume or fills it.
func shelfStatusCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, shelfUsage)
		return 2
	}
	var req struct {
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&req); err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host shelf status: the request on stdin:", err)
		return 2
	}
	cfg, err := parseConfig(req.Config)
	if err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host shelf status:", err)
		return 1
	}
	vm := newLayer(cfg)
	if _, ok := vm.(shelfer); !ok {
		fmt.Fprintf(stderr, "potluck-driver-tart host shelf status: vm=%s has no shelf (only vm=oci does)\n", cfg.VM)
		return 1
	}
	fmt.Fprint(stdout, shelfStatus(&drv{cfg: cfg, vm: vm}))
	return 0
}

// shelfStatus is the status text for d's binding.
func shelfStatus(d *drv) string {
	s := d.cfg.Shelf
	out := "shelf: "
	if sh, ok := d.shelfDescribe(); !ok {
		out += fmt.Sprintf("no %s volume (boxes have no shelf)\n", ShelfVolume)
	} else if why, _ := sh["why"].(string); why != "" {
		out += fmt.Sprintf("%s, %s\n", sh["path"], why)
	} else {
		out += fmt.Sprintf("%s, %v bytes, %v files\n", sh["path"], sh["bytes"], sh["files"])
	}
	fill := s.Fill
	if s.MaxGB == 0 {
		fill += " (but shelf_max_gb is 0, so nothing fills)"
	}
	out += "fill: " + fill + "\n"
	out += fmt.Sprintf("limits: shelf_max_gb=%d shelf_keep_s=%d shelf_fill_mbit=%d shelf_fill_delay_s=%d persist_min_free_gb=%d volume_max_gb=%d\n",
		s.MaxGB, s.KeepS, s.FillMbit, s.FillDelayS, d.cfg.PersistMinFreeGB, d.cfg.VolumeMaxGB)
	q := shelfQueue(d.cfg.StateDir)
	pending := len(slices.DeleteFunc(slices.Clone(q), func(x shelfQueued) bool { return x.offset >= x.size }))
	out += fmt.Sprintf("queue: %d box files, %d with unread wants, %d unread bytes of %d allowed\n", len(q), pending, shelfQueueBytes(d.cfg.StateDir), shelfQueueMax)
	return out
}

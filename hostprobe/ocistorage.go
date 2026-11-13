package hostprobe

// The oci storage keys potluck host sets: --shelf-gb (shelf_max_gb),
// --shelf-fill (shelf_fill) and --volume-max-gb (volume_max_gb). All the sizes
// of one engine share one potluck-cache volume and each applies its own limits
// to all of it (the smallest wins), so host gives every size it binds the same
// values and refuses a run that would leave another size of the engine, in the
// scope it binds in (o.Earlier), with different ones; potluck host shelf status
// reads both scopes and warns. A re-run without the flags keeps the earlier values, as it
// keeps admission keys.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/vmhost"
)

// ociStorageKeys are the keys, with the flag that sets each. engine says the
// key's value applies to the engine's one shelf volume, whichever size reads
// it (the smallest wins), so every size of the engine must carry it alike;
// volume_max_gb is each size's own, so only this share's own sizes (its name
// prefix) are kept alike.
var ociStorageKeys = []struct {
	key, flag string
	engine    bool
}{
	{"shelf_max_gb", "--shelf-gb", true}, {"shelf_fill", "--shelf-fill", true}, {"volume_max_gb", "--volume-max-gb", false},
}

// shelfLimitKeys are the other shelf limits that, set on one size, apply to the
// engine's shelf: host does not set them, and status warns where they differ.
var shelfLimitKeys = []string{"shelf_keep_s"}

// shelfTuningKeys are kept as bound by hand and never set by a flag: a re-run
// would otherwise put them back to their defaults (a guarded uplink's
// shelf_fill_mbit, a longer keep time).
var shelfTuningKeys = []string{"shelf_keep_s", "shelf_fill_mbit", "shelf_fill_delay_s"}

// engineOf is the container CLI a binding runs on (the driver's default is
// docker).
func engineOf(t computeapi.RunTarget) string { return cmp.Or(t.Config["oci_cli"], "docker") }

// storageGBMax bounds --shelf-gb and --volume-max-gb (100 TB).
const storageGBMax = 100000

// OCIStorage is the storage keys a share binds on every size, and where they
// came from.
type OCIStorage struct {
	Keys map[string]string `json:"keys"`
	// From names the earlier binding the keys not given as flags were kept from.
	From string `json:"kept_from,omitempty"`
	// Volume is the command that makes the shelf's volume (made only when it is
	// missing); set when shelf_max_gb is above 0.
	Volume []string `json:"create_volume,omitempty"`
}

// ociStorage resolves the keys for a share of sizes on engine cli. It is nil
// when there is nothing to bind: no flag was given and no earlier binding of
// the engine carries one.
func ociStorage(o ShareOptions, layer, cli string, sizes []Size) (*OCIStorage, error) {
	given := map[string]string{}
	if o.ShelfGB != nil {
		given["shelf_max_gb"] = strconv.Itoa(*o.ShelfGB)
	}
	if o.ShelfFill != "" {
		given["shelf_fill"] = o.ShelfFill
	}
	if o.VolumeMaxGB != nil {
		given["volume_max_gb"] = strconv.Itoa(*o.VolumeMaxGB)
	}
	if layer != "oci" {
		if len(given) > 0 {
			return nil, fmt.Errorf("--shelf-gb, --shelf-fill and --volume-max-gb are for the oci layer (the shelf is a container volume): %s boxes have none", layer)
		}
		return nil, nil
	}
	if o.ShelfGB != nil && (*o.ShelfGB < 0 || *o.ShelfGB > storageGBMax) {
		return nil, errors.New("--shelf-gb: want GiB, 1..100000, or 0 to fill nothing")
	}
	if o.VolumeMaxGB != nil && (*o.VolumeMaxGB < 0 || *o.VolumeMaxGB > storageGBMax) {
		return nil, errors.New("--volume-max-gb: want GiB, 1..100000, or 0 for no volumes")
	}
	switch o.ShelfFill {
	case "", "off", "seed", "boxes":
	default:
		return nil, errors.New("--shelf-fill: want off, seed or boxes")
	}
	if o.EarlierUnread != "" && !o.DryRun && len(given) < len(ociStorageKeys) { // a dry run binds nothing
		return nil, fmt.Errorf("could not read this machine's bindings (%s): pass --shelf-gb, --shelf-fill and --volume-max-gb, so a re-run never drops the shelf or volume limits an earlier share bound, or try again", o.EarlierUnread)
	}
	bound := map[string]bool{}
	for _, s := range sizes {
		bound[o.NamePrefix+"-"+s.Name] = true
	}
	var engine []computeapi.RunTarget // every oci size of this engine on this machine, by name
	for _, t := range o.Earlier {
		if t.Kind == "box" && t.Host == "" && t.Config["vm"] == "oci" && engineOf(t) == cli {
			engine = append(engine, t)
		}
	}
	slices.SortFunc(engine, func(a, b computeapi.RunTarget) int { return strings.Compare(a.Name, b.Name) })
	eff := storageValue
	st := &OCIStorage{Keys: map[string]string{}}
	for _, k := range ociStorageKeys {
		group := engine // the sizes that must carry this key alike
		if !k.engine {  // this run's own sizes: each size counts its own, and a size left out keeps its own
			group = slices.DeleteFunc(slices.Clone(engine), func(t computeapi.RunTarget) bool { return !bound[t.Name] })
		}
		if v, ok := given[k.key]; ok {
			st.Keys[k.key] = v
			for _, t := range group { // a size this run leaves alone keeps its own
				if k.engine && !bound[t.Name] && eff(t, k.key) != v {
					return nil, fmt.Errorf("%s %s: binding %s is not in this run and has %s=%s, and every size on one engine must carry the same value (they share one potluck-cache volume and the smallest wins): bind it in this run too (--sizes), or give it the same value",
						k.flag, v, t.Name, k.key, eff(t, k.key))
				}
			}
			continue
		}
		var first *computeapi.RunTarget
		kept := false
		for i, t := range group {
			if first == nil {
				first = &group[i]
			} else if eff(t, k.key) != eff(*first, k.key) {
				return nil, fmt.Errorf("bindings %s and %s differ in %s (%s and %s): pass %s to say which", first.Name, t.Name, k.key, eff(*first, k.key), eff(t, k.key), k.flag)
			}
			kept = kept || t.Config[k.key] != ""
		}
		if first != nil && kept {
			st.Keys[k.key] = eff(*first, k.key)
			if st.From == "" {
				st.From = first.Name
			}
		}
	}
	for _, k := range shelfTuningKeys { // kept from the first of this run's sizes that has it
		for _, t := range engine {
			if v := t.Config[k]; v != "" && bound[t.Name] {
				st.Keys[k] = v
				if st.From == "" {
					st.From = t.Name
				}
				break
			}
		}
	}
	if len(st.Keys) == 0 {
		return nil, nil
	}
	gb := func() int {
		v, ok := st.Keys["shelf_max_gb"]
		if !ok {
			v, _ = vmhost.StorageKeyDefault("shelf_max_gb")
		}
		n, _ := strconv.Atoi(v)
		return n
	}()
	if (o.ShelfFill == "seed" || o.ShelfFill == "boxes") && gb == 0 {
		return nil, fmt.Errorf("--shelf-fill %s: the shelf is off (shelf_max_gb is 0): pass --shelf-gb N, N above 0", o.ShelfFill)
	}
	if gb > 0 {
		st.Volume = []string{cli, "volume", "create", vmhost.ShelfVolume}
	}
	return st, nil
}

// storageValue is a binding's value of one of the storage keys, the default
// when it leaves the key out.
func storageValue(t computeapi.RunTarget, key string) string {
	if v := t.Config[key]; v != "" {
		return v
	}
	d, _ := vmhost.StorageKeyDefault(key)
	return d
}

// StorageDisagreements says, in plain words, where the oci box bindings of ts
// (this machine's) that share one engine carry different storage keys: the
// shelf volume is shared, so the smallest of each limit wins. potluck host
// binds them alike; a hand-edited binding, or a size that failed a run, can
// differ.
func StorageDisagreements(ts []computeapi.RunTarget) []string {
	byEngine := map[string][]computeapi.RunTarget{}
	for _, t := range ts {
		if t.Kind == "box" && t.Host == "" && t.Config["vm"] == "oci" {
			byEngine[engineOf(t)] = append(byEngine[engineOf(t)], t)
		}
	}
	var out []string
	for _, cli := range slices.Sorted(maps.Keys(byEngine)) {
		group := byEngine[cli]
		slices.SortFunc(group, func(a, b computeapi.RunTarget) int { return strings.Compare(a.Name, b.Name) })
		keys := slices.Clone(shelfLimitKeys)
		hint := map[string]string{"shelf_keep_s": "set it alike in each binding"}
		for _, k := range ociStorageKeys {
			if k.engine {
				keys = append(keys, k.key)
				hint[k.key] = "potluck host " + k.flag + " sets every size alike"
			}
		}
		for _, k := range keys {
			for _, t := range group[1:] {
				if storageValue(t, k) != storageValue(group[0], k) {
					out = append(out, fmt.Sprintf("%s=%s on %s but %s on %s (one engine, one shelf volume: the smallest limit wins; %s)",
						k, storageValue(group[0], k), group[0].Name, storageValue(t, k), t.Name, hint[k]))
					break
				}
			}
		}
	}
	return out
}

// ensureShelfVolume makes the shelf's volume when the engine has none.
func ensureShelfVolume(ctx context.Context, run Runner, st *OCIStorage) error {
	if len(st.Volume) == 0 {
		return nil
	}
	cli := st.Volume[0]
	if r, err := run.Run(ctx, cli, "volume", "inspect", vmhost.ShelfVolume); err == nil && r.Exit == 0 {
		return nil
	}
	r, err := run.Run(ctx, cli, st.Volume[1:]...)
	switch {
	case err != nil:
		return fmt.Errorf("make the shelf volume (%s): %v", strings.Join(st.Volume, " "), err)
	case r.Exit != 0:
		return fmt.Errorf("make the shelf volume (%s): exit %d: %s", strings.Join(st.Volume, " "), r.Exit, strings.TrimSpace(r.Stderr))
	}
	return nil
}

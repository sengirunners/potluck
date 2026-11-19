package hostprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/imagepins"
)

// The block test (potluck isolation test, docs/isolation.md) proves a box
// binding's isolation on this machine: conformance's net group, run on the
// binding's own driver and config, shows a box cannot reach this machine on
// a port that is open, the LAN's router, the tailnet, or another box at the
// same time. Its result is recorded under the state directory, and doctor
// shows it beside the binding.

// BlockTestOptions is one box binding's block test.
type BlockTestOptions struct {
	Binding    string
	DriverName string // the binding's driver ("tart"), for the record's fingerprint
	// DriverSHA256 is the driver program's pinned sha256 (DRIVERS.md), for
	// the fingerprint: the walling-off lives in the program, so a pass holds
	// only for that file. Empty fails the test.
	DriverSHA256 string
	Driver       string            // the driver program, the file that sum is of
	Config       map[string]string // the binding's driver config, as bound
	Images       []string          // the binding's pinned images, all sent as config images
	Suite        string            // the conformance program; default as share's
	// NetForbid are more host:port targets the owner names (a NAS, a
	// printer): each must open from here, as for share.
	NetForbid []string
	Timeout   time.Duration // default 15 minutes
	// HostAddrs are this machine's addresses (default HostAddrs()). Tests
	// set them; an empty list fails the test, since nothing would show the
	// box cannot reach this machine. Link-local ones are dropped (the suite
	// skips them; --net-peers covers link-local neighbours).
	HostAddrs []string
}

// BlockTestResult is what one block test showed, as recorded.
type BlockTestResult struct {
	Binding string    `json:"binding"`
	At      time.Time `json:"at"`
	Passed  bool      `json:"passed"`
	// Fingerprint is BindingFingerprint of the binding as tested: a record
	// counts only while the binding still has it.
	Fingerprint string `json:"fingerprint"`
	// Forbid are the addresses past this machine the box had to fail to
	// reach (this machine's own addresses are in Command, on a port the test
	// listened on).
	Forbid  []string     `json:"forbid"`
	Command []string     `json:"command"`
	Checks  []BlockCheck `json:"checks,omitempty"`
	// Notes are what a pass did not cover, in plain words (the forward
	// path, when only the owner's own targets were checked).
	Notes []string `json:"notes,omitempty"`
	// Error is why it did not pass, in plain words ("" when it passed).
	Error string `json:"error,omitempty"`
}

// BlockCheck is one conformance check of the run (its JSON result).
type BlockCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Group   string `json:"group,omitempty"`
}

// bindOnly reports a config key a size's binding carries but its
// conformance run leaves out; Check says why for each: runner_dir (so the
// run's boxes, which bring no jit_config, get the network a runner box
// gets), the reclaim policy, the image's name and one-at-a-time slots (the
// peers check runs two boxes at once). TestSuiteConfigMatchesShare holds
// Check to this list.
func bindOnly(k string) bool {
	_, slot := OneAtATimeConfig[k]
	return slot || k == "runner_dir" || k == "reclaim_idle_s" || k == "image_name" || k == imagepins.ImageDefaultKey
}

// suiteConfig is a bound box binding's config as its conformance run takes
// it: Check's run config, without the bind-only keys and with images.
func suiteConfig(bound map[string]string, images []string) map[string]string {
	cfg := map[string]string{}
	for k, v := range bound {
		if !bindOnly(k) && k != "images" {
			cfg[k] = v
		}
	}
	cfg["images"] = strings.Join(images, ",")
	return cfg
}

// BindingFingerprint names what a block test tested: the driver, its
// program's sha256, and the config and images its run took. A binding
// changed since (re-bound with another layer, image or network, or its
// driver upgraded and re-pinned) no longer has its record's fingerprint.
func BindingFingerprint(driver, driverSHA256 string, config map[string]string, images []string) string {
	cfg := suiteConfig(config, images)
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "driver=%q\n", driver)
	fmt.Fprintf(h, "driver_sha256=%q\n", driverSHA256)
	for _, k := range keys {
		fmt.Fprintf(h, "%q=%q\n", k, cfg[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BlockTest runs the block test. It passes only when the run exited 0 and
// every check of the net group ran and passed, none skipped: a check that
// could not run proves nothing, so it fails the test.
func BlockTest(ctx context.Context, run Runner, o BlockTestOptions) BlockTestResult {
	res := BlockTestResult{Binding: o.Binding, At: time.Now().UTC(), Fingerprint: BindingFingerprint(o.DriverName, o.DriverSHA256, o.Config, o.Images)}
	fail := func(format string, a ...any) BlockTestResult {
		res.Passed, res.Error = false, fmt.Sprintf(format, a...)
		return res
	}
	if o.DriverSHA256 == "" {
		return fail("potluck-driver-%s has no pinned sha256, so the record could not say which program it tested", o.DriverName)
	}
	if len(o.Images) == 0 {
		return fail("%s has no image to test", o.Binding)
	}
	if err := checkAddrs(o.NetForbid); err != nil {
		return fail("%v", err)
	}
	all := o.HostAddrs
	if all == nil {
		all = HostAddrs()
	}
	var addrs []string
	for _, a := range all {
		if ip := net.ParseIP(a); ip != nil && !ip.IsLinkLocalUnicast() {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return fail("this machine has no address but loopback, so nothing would show a sandbox cannot reach it")
	}
	fwd := forwardTargets()
	res.Forbid = append(append([]string(nil), o.NetForbid...), fwd...)
	if len(res.Forbid) == 0 {
		return fail("%s, so nothing would show a sandbox cannot reach the LAN or the tailnet", forwardUnchecked)
	}
	if len(fwd) == 0 {
		res.Notes = append(res.Notes, forwardUnchecked)
	}
	nets, stop, err := netArgs(ShareOptions{NetForbid: res.Forbid, HostAddrs: addrs})
	if err != nil {
		return fail("listen for the host-address checks: %v", err)
	}
	defer stop()
	suite := o.Suite
	if suite == "" {
		suite = defaultSuite(SystemFacts{}.LookPath)
	}
	res.Command = append(append(conformanceArgs(suite, o.Driver, suiteConfig(o.Config, o.Images)), "--only", "net", "--json"), nets...)
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := run.Run(cctx, res.Command[0], res.Command[1:]...)
	if err != nil {
		return fail("conformance did not run: %v", err)
	}
	if err := json.Unmarshal([]byte(r.Stdout), &res.Checks); err != nil {
		return fail("conformance's answer is not its JSON results (exit %d): %s", r.Exit, oneLine(r.Stderr))
	}
	var net, bad []string
	for _, c := range res.Checks {
		if c.Group == "net" {
			net = append(net, c.Name)
		}
		switch {
		case !c.OK:
			bad = append(bad, "failed: "+c.Name+": "+c.Detail)
		case c.Skipped && c.Group == "net":
			bad = append(bad, "did not run, so proves nothing: "+c.Name+": "+c.Detail)
		}
	}
	switch {
	case len(bad) > 0:
		return fail("%s", strings.Join(bad, "; "))
	case r.Exit != 0:
		return fail("conformance exited %d: %s", r.Exit, oneLine(r.Stderr))
	case len(net) == 0:
		return fail("conformance ran no network check")
	}
	res.Passed = true
	return res
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// bindingFile is a binding name safe as one file name.
var bindingFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)

func blockTestPath(dir, binding string) (string, error) {
	if !bindingFile.MatchString(binding) || strings.Contains(binding, "..") {
		return "", fmt.Errorf("binding name %q cannot name a file", binding)
	}
	return filepath.Join(dir, "isolation", binding+".json"), nil
}

// RecordBlockTest keeps r as its binding's latest block test, under dir
// (the state directory), replacing the one before.
func RecordBlockTest(dir string, r BlockTestResult) error {
	p, err := blockTestPath(dir, r.Binding)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".block-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// LoadBlockTest is binding's latest recorded block test; ok false when
// none was recorded. A record that cannot be read is an error, never a pass.
func LoadBlockTest(dir, binding string) (r BlockTestResult, ok bool, err error) {
	p, err := blockTestPath(dir, binding)
	if err != nil {
		return r, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return BlockTestResult{}, false, fmt.Errorf("%s: %v", p, err)
	}
	if r.Binding != binding {
		return BlockTestResult{}, false, fmt.Errorf("%s records binding %q", p, r.Binding)
	}
	return r, true, nil
}

# Adding a VM layer

A VM layer is how Potluck's VM driver (`vmhost/`, built as `cmd/potluck-driver-tart`) talks to one hypervisor or
sandbox runtime. Tart (`vmhost/vm.go`), Incus (`vmhost/incus.go`), OCI runtimes such as Kata and gVisor
(`vmhost/oci.go`) and cloud-hypervisor (`vmhost/cloudhv.go`, [cloudhv.md](cloudhv.md)) are layers. To support another hypervisor, write one more: a Go file that implements one interface
and is listed in one map. The driver does the rest.

## What a layer is, and is not

A layer is a thin wrapper over a CLI or API that can: make a copy of an image, start it with nothing of the host
attached, run a command in it, list what it made, and delete it. It holds no state of its own about runs.

A layer is not a scheduler, a queue or a lease manager, and it does not talk to the protocol (`docs/protocol.md`).
It does not decide how many VMs run, when one expires, or what a repeated request means. Those belong to the driver
and are listed under "What you get for free".

The binding's config picks the layer with `vm=<name>` (default `tart`). Every layer is built from the config
(`config` in `vmhost/config.go`), so it is created fresh in the driver process and again in each run's supervisor,
from the config saved with the run.

## The interface

`vmLayer` in `vmhost/layer.go` has six methods. The comment above it is the contract; this is the same list with
what the caller does.

| method | must do |
| --- | --- |
| `OSOf(name string) (string, error)` | Answer `"linux"` or `"darwin"` for an image name or a VM name, or an error when the image is not there. `describe` calls it with the binding's image (a missing image makes the driver report itself unavailable), and `acquire` calls it with the image asked for. On a Mac the slot count also calls it with the name of each running VM. |
| `Clone(image, vm string, cpu, memory int) error` | Make a VM named `vm` from `image` that shares nothing with any other VM: a fresh copy of the disk, its own MAC address. Size it to `cpu` and `memory` (MiB) only when they are above 0; 0 means keep the image's. It must not start it. |
| `Boot(vm string, isolated bool, log io.Writer) (cmd *exec.Cmd, held io.WriteCloser, err error)` | Start the VM headless, with no shared directory, clipboard, audio or display from the host. `cmd` is a started host process, in its own process group, that lives as long as the VM: killing the group powers the VM off, and its exit tells the supervisor the VM is gone. `held` is the write end of a pipe the layer reads. When it closes (the supervisor exited or was killed) the layer must power the VM off and delete it, with no help from anyone. Write the layer's own output to `log`. |
| `Exec(vm string, stdin io.Reader, stdout, stderr io.Writer, argv ...string) *exec.Cmd` | Return a command, not yet started, that runs `argv` in the guest with no SSH and no credential. Stdout and stderr stay apart, stdin is passed through, and the command's exit code is the guest command's. It must fail (not hang) when the VM is not running. |
| `List() ([]vmInfo, error)` | The VMs this layer made on this host: `Name`, `Running`, and `Local` (made here, not a pulled image). The sweep uses it to find what a dead supervisor left. |
| `Destroy(vm string, runPID int)` | Power the VM off and delete it. `runPID` is `Boot`'s process (0 when no supervisor of ours holds it). It has no error return and must be safe to call twice, and on a VM that is already gone. |

What the caller guarantees:

- VM names come from the driver: `potluck-<8 hex digits>-<run id>`, the hex being a hash of the state directory
  (`vmPrefixNew` in `vmhost/store.go`; names made before the rename start `zmrt-`). Image names are the owner's.
  `List` should return every VM the layer made, and the driver treats only names with its prefix as its own
  (`ownVM`), so a layer never has to filter by owner itself, but it must not delete what it did not make.
- `Clone` runs once per VM, before `Boot`. `Boot` is called with `isolated` true unless the owner set
  `network=nat`, the unsafe development setting (`vmhost/supervise.go`, `run`).
- After `Boot`, the supervisor calls `Exec(vm, ..., "true")` every 250 ms until it succeeds or `boot_timeout_s`
  (default 90, at most 100) passes (`waitReady` in `vmhost/layer.go`); a try still running then is ended. A VM whose
  `Exec` never succeeds fails the run with "the guest agent did not answer", or, when a try wrote to its stderr,
  with "the guest ran no command within the boot timeout" and the start of what the last such try wrote.
- The guest must have `sh`, and `git` for the checkout (`checkout` in `vmhost/supervise.go`). A checkout runs
  `git fetch` in the guest, or, when the host fetches (`fetch=host`, or a local remote), the host streams a git
  bundle in on the stdin of an `Exec`.
- `Destroy` may be called from the supervisor at the end of every run, from `release`, from the expiry path, and by
  the sweep on a VM whose supervisor died. Expect several in a row for one VM.

## The fail-closed isolation rule

`isolated` is the credential-free network of the driver protocol (`docs/protocol.md`; how the VM driver builds it:
`docs/network.md`): the guest reaches the internet and DNS and never the host's LAN, the host itself, a tailnet or
link-local metadata. A layer that cannot guarantee that must refuse `isolated`:
return an error from `Boot` when `isolated` is true and the layer cannot enforce it. It must not start the VM and
hope. A layer that can only isolate in some setups checks the setup before every boot and refuses when it cannot
tell. Anything the layer cannot read as safe counts as unsafe. Incus is the worked case: it reads the VM's expanded
config before `Boot` and again after the start, and refuses a NIC without port isolation or an ACL that blocks the
LAN (`fenced` in `vmhost/incus.go`, `vmhost/incus_acl.go`).

Do not widen this to make a test pass. A layer that cannot isolate is still allowed, but only for bindings with
`network=nat`, which `describe` reports with isolation `none`.

## What you get for free

None of this is the layer's job. Each lives in `vmhost/`:

| you get | where |
| --- | --- |
| One detached supervisor per run, which clones, boots, waits for the guest, checks out the commit, runs the script or the box, and deletes the VM when the run ends | `supervise.go` (`supervise`, `sup.run`, `sup.watch`) |
| Lease expiry: `expires_at` is enforced by the supervisor itself with no call from the daemon, and `renew` moves it within the lifetime cap (`ttl_max_s`) | `supervise.go` (`watch`), `driver.go` (`renew`) |
| Idempotent acquire: the same `request_id` returns the same run, and a changed request under an old `request_id` is refused | `driver.go` (`acquire`) |
| Cleanup after a crash: a sweep ends the run of a supervisor that died and deletes this state directory's VMs that no live run holds, using your `List` and `Destroy` | `driver.go` (`sweep`) |
| Slots per OS (`linux_slots`, `macos_slots`) | `driver.go` (`free`), `config.go` |
| Admission: with `admission_url` set, every VM first takes a lease from the host's admission controller, renews it while the VM lives, and ends the VM if the lease is lost | `admission.go`, `supervise.go` (`takeRoom`) |
| The reclaim policy (`window`, `windows`, `reclaim_idle_s`) and the host override (`host set`, drains) | `reclaim.go`, `hostmode.go`, `drain.go`, `hostset.go` |
| Boxes: `exec`, background processes, output paging, signals | `exec.go`, `procs.go`, `box.go` |
| The wire protocol, the state records, and describe | `driver.go`, `store.go` |
| Warm boxes (`warm`) | `warm.go` |

## Optional interfaces

A layer may also implement these. The driver looks for each with a type assertion and turns the feature off when
it is missing:

| interface | gives | where |
| --- | --- | --- |
| `layerConfig` | the layer reads and checks its own `vm=<name>` config keys | `vmhost/layer.go` |
| `loginCloser` | argv run in each guest before anything else, to close a login the image keeps open | `vmhost/layer.go` |
| `ttyLayer` | terminals (`input` verb) | `vmhost/pty.go` |
| `rawTTYLayer` | a `ttyLayer` whose exec only carries bytes to a terminal in the guest: the host terminal is raw before it starts | `vmhost/pty.go` |
| `keeper` | kept disks (`persist`, `stop` and `start`) | `vmhost/persist.go` |
| `volumer` | named volumes | `vmhost/volume.go` |
| `diskDir` | the directory the persist free-space floor measures | `vmhost/persist.go` |

Start without them. Each one adds capabilities to `describe`, and the conformance groups for them (`pty`,
`persist`) only run when the driver lists the capability.

## Config keys of your own

A layer that needs keys (a binary path, a remote, a project) implements `layerConfig`:

```go
type layerConfig interface {
	ParseConfig(m map[string]string, c *config) (bad []string)
}
```

`parseConfig` builds your layer from the config read so far, and, if it implements `layerConfig`, calls
`ParseConfig` with the raw binding keys `m` and the config `c` to fill. Set your fields on `c`, apply defaults, and
return one plain-words message per bad or missing key (say the key's name). Those messages join the rest into one
`bad_request` refusal, so an owner sees every problem at once, before anything runs. The call happens only for
bindings with `vm=<your name>`, in the driver and again in each supervisor, so a layer's keys can never refuse
another layer's binding.

Incus is the worked example: `incusCLI.ParseConfig` in `vmhost/incus.go`, tested in `vmhost/layer_config_test.go`
and `TestIncusConfig` in `vmhost/incus_test.go`. Two things are still done by hand for a new field:

- add the field to `config` in `vmhost/config.go`, and
- write it back out in `(*drv).rawConfig` in `vmhost/driver.go` (the `if c.VM == "incus"` block near line 476 is the
  model: add an `if c.VM == "yours"` block next to it), since the supervisor rebuilds its config from that saved
  map. A key that is not written there is lost between `acquire` and the run.

The older `oci_*` and `root` keys are still checked inside `parseConfig` itself; they work and need not move.

## Register it

In `vmhost/layer.go`, add your layer to `layers`:

```go
var layers = map[string]func(c config) vmLayer{
	...
	"yours": func(c config) vmLayer { return yourCLI{c.YoursBin} },
}
```

The factory must have no side effects: it is called more than once, including while config is being checked. Keep
it to building a struct. Do the real work (and report errors) in the methods.

`vm=yours` then selects it. An unknown name is refused with the list of layers (`layerNames`).

Also add the layer's name to the list of layers in `TestNatDescribesIsolationNone` (`vmhost/net_test.go`, near line
18), which checks that every layer describes isolation `none` under `network=nat` and `ephemeral` otherwise.

`hostprobe.Supported` and `LayerOrder` (`hostprobe/probe.go`) decide which layers the share and doctor commands
offer; adding a layer there is a separate, optional step.

## Testing a layer

Never start a real VM in a test, and never touch the real home directory (`AGENTS.md`). `vmhost/fake_test.go` sets
`HOME` to a temp directory in `TestMain`.

**A fake CLI script.** The pattern is a shell script that stands in for the layer's CLI, written to `t.TempDir()`
and driven by an environment variable that points at a state directory. Read the two existing ones:

- `fakeIncus` and `newFakeIncus` in `vmhost/incus_test.go`: a VM is a directory, `exec` runs on the host, a file
  named `down` makes every call fail as an unreachable server. `TestIncusLayer` checks the exact argv sent to the
  CLI, exit codes and stdin passing through `Exec`, `List` ignoring VMs it did not make, and `Destroy` called twice.
- `fakeOCI` in `vmhost/oci_test.go`, with `TestOCILayer` and `TestOCIBootEndsWithTheContainer`.

Test at least: `Clone` and `Boot` send the arguments you expect; `Exec` passes the exit code and stdin; `List` shows
only your VMs; `Destroy` twice is fine; closing the `held` pipe deletes the VM; a refusal on an unsafe `isolated`.
There is no fake Tart: Tart's code is exercised through the fake layer below.

**The fake layer and the whole supervisor.** `dirLayer` in `vmhost/fake_test.go` is a layer whose VMs are
directories and whose guest is the host. It is registered under the name `fake` in that file's `init`. Run
the supervisor, leases and expiry over the fake with:

```sh
go test -p 2 ./vmhost/ -run 'TestConformanceOverAFakeLayer|TestBoxConformanceOverAFakeLayer'
```

Those tests run the whole conformance suite against the driver with `vm=fake`. Copy `fakeBinding` and one of them
into a test of yours, swap in your layer by registering it in `layers` from a `_test.go` file, and point it at your
fake CLI.

Then, before opening a PR:

```sh
gofmt -l .                  # prints nothing
go vet ./vmhost/...
go test -p 2 ./vmhost/...
```

## Conformance against a real hypervisor

The suite is described in `docs/conformance.md`. The driver you test is the same `potluck-driver-tart` with your
layer chosen in the config. Build both, and run it on a machine that has your hypervisor, with a throwaway state
directory:

```sh
go build ./cmd/potluck-driver-tart ./cmd/potluck-conformance
./potluck-conformance ./potluck-driver-tart --kind run --remote --ref main --commit <older pushed commit with probe.sh> \
  --repo <remote> --script probe.sh --config vm=yours --config image=<your base image> --config remote=<remote> \
  --config state_dir=/tmp/yours-state
```

`./potluck-conformance --print-probe > probe.sh` prints the probe script; commit it to a git repository, which is
`<remote>`, and keep one commit on top of it so that `--commit` is not the tip (`docs/conformance.md`). Use
`--kind box` to run the box groups as well. `cmd/potluck-driver-tart/README.md` ("Check") shows the full
invocation used for Tart, including `--reclaim-config`, and the network flags (`--net-allow`, `--net-forbid`,
`--net-mail`) that prove isolation from inside the guest. Give the network flags: a layer that passes without them
has not shown that `isolated` holds. Report which checks were SKIP in the PR. A skip is not a pass.

Run `./potluck-conformance --help` for every flag. The suite makes and ends real VMs.

## PR checklist

- [ ] One new file `vmhost/<name>.go` with the layer, and one line in `layers` (`vmhost/layer.go`). Nothing else
      in the supervisor changed, or the PR says why.
- [ ] All six `vmLayer` methods, each safe to call as the contract says (`Destroy` twice, `Exec` on a dead VM).
- [ ] `Boot` refuses `isolated` when the layer cannot enforce the isolated network, and a test shows it.
- [ ] The VM does not outlive the supervisor: a test closes `held` and shows the VM deleted.
- [ ] Config keys, if any, are checked in `ParseConfig` with plain-words errors, and written back in `rawConfig`.
- [ ] A test with a fake CLI script, as above; no real VM, no real home directory, no new dependency.
- [ ] `gofmt -l .` prints nothing; `go vet ./vmhost/...` and `go test -p 2 ./vmhost/...` pass.
- [ ] Conformance output against the real hypervisor, with the exact command, the OS and hypervisor version, and
      the list of SKIPs.
- [ ] Setup for the layer (what the owner installs and which keys to set) in `cmd/potluck-driver-tart/README.md`.
- [ ] Commits signed off (`git commit -s`).

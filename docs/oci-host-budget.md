# Kata host resource budgets

Kata's workload CPU and memory flags do not bound all host VMM processes.
A Kata binding therefore requires an owner-supplied `oci_host_profile` (the
`potluck host` flag is `--oci-host-profile`). Missing or unverifiable profiles
refuse acquisition before admission. This does not change the controller API,
its capacity policy, Docker's default runtime, or the workload's advertised size.

The profile is an absolute JSON file on the lending host:

```json
{
  "runtime": "kata-qemu",
  "runtime_config": "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml",
  "runtime_config_sha256": "<sha256 of the selected configuration>",
  "runtime_binary_sha256": "<sha256 of the registered runtime binary>",
  "cpu_overhead": 1,
  "memory_overhead_mib": 512,
  "cleanup_reserve_s": 10
}
```

These allowances are owner configuration, not a measured minimum or an automatic
recommendation. They include the native configured VM overhead and host runtime
processes once. For example, workload c1m2 with the illustrated allowances charges
and enforces a total of 2 CPU and 2560 MiB; the OCI workload remains 1 CPU and
2048 MiB. A native topology rounded to two vCPUs does not itself require a two-CPU
host quota. The existing admission API uses integer CPU units, so this profile's
explicit host ceiling is also integral.

The selected Docker runtime registration, runtime binary hash and native config
hash must match. The supported native configuration uses static sandbox sizing,
sandbox-only cgroups and no vCPU pinning. Its configured CPU/memory overhead must
fit inside the profile allowance, with additional memory for host processes.
Configuration drift refuses clone/boot. This initial native parent path requires
Docker's systemd cgroup-v2 driver, Bash 4 or newer, and the operator's existing `sudo -n` access;
it does not install privileges or change host runtime configuration.

One immutable total allocation is stored with each run. Descriptor availability,
local CPU/RAM lending caps, the existing admission request and finite native
systemd slice all use it. Existing allocations are not recalculated after a
profile edit. The native parent is unique to the state directory, VM and run
generation; collisions fail rather than replace a unit. CPU and memory limits
are read back before Docker receives `--cgroup-parent`.

The existing OCI stdin guard also has a hard monotonic deadline. Only successful
controller renewals send a new absolute deadline. Cleanup starts the configured
10–30 seconds before confirmed controller expiry; a lease too short for boot and
cleanup refuses startup. Supervisor death closes the pipe; a stopped supervisor
cannot renew the guard. Native descendant kill and unit stop precede bounded
Docker removal, so a hanging Docker CLI cannot delay the native kill. An attach
exit cannot cancel cleanup. Admission is deliberately not released unless the
owned parent is absent or reports empty. Local slot accounting also retains a
populated parent after a record ends.

Outer named volumes, the fixed outer tool-cache mount, unleased warm boxes and persistent restart are unavailable for these profiles:
a kept Docker container cannot be assigned a fresh cgroup parent at restart.
The driver reports this limitation instead of claiming an unbounded restart is
safe. This source change still requires normal CI, independent review and
isolated runtime acceptance before enabling a pool.

## Live engine identity

Registration names are aliases. Before admission the producer resolves the
selected Docker context/host, requires a local Unix endpoint, and verifies the
root Docker service and its socket listener in the same host namespaces. A
systemd-activated socket with peer PID 1 is accepted only with the exact active
socket/service association and the listener inode held by dockerd. No socket,
service, runtime registration or privilege is changed.

The producer reads the selected registration from live `/info`, verifies the
actual program's version identity, and requires a bounded profile for Kata even
when its alias is `runsc`. Docker 29's `/info` omits shim-v2 type/options. For
that supported case, the current service invocation's latest successful reload
journal record provides Docker's sanitized *active* configuration. Its PID,
boot, invocation and monotonic ordering must match; missing, malformed, pending,
failed or visibly incomplete journal evidence refuses. Only the selected runtime
fields remain in memory; full configuration is never reported. No reload is
needed when the existing record is complete. Older or differently configured
engines without enough evidence refuse with the missing prerequisite.

The run stores the verified local endpoint, engine lifecycle and selected
registration identity. New allocation/clone/boot revalidate it; existing handle
transport uses the stored endpoint and does not repeat native discovery for
output/port pages. Context selection changes cannot redirect those operations.
The host operator is trusted: these checks detect observed drift and pending
reloads, and do not claim an atomic transaction against concurrent root changes.
The native resource parent and hard lifetime remain the enforcement boundary.
This evidence path currently requires the systemd-managed local Docker engine;
other OCI CLIs do not provide this verification route.

Implementation sources:
[Docker runtime info](https://github.com/moby/moby/blob/docker-v29.1.3/daemon/info_unix.go),
[successful reload snapshot](https://github.com/moby/moby/blob/docker-v29.1.3/daemon/command/daemon.go#L507-L518).


Outer-volume measurement needs its own independently admitted finite helper,
including quota checks while a box runs and final/dead-supervisor measurements
after its lease ends. Until that lifecycle is supplied, the bounded backend
advertises no outer-volume capability and refuses before acquiring a volume.
The adapter also refuses direct/missing-profile/Kata-alias helper entry paths;
old records are marked unknown and count against their existing quota. It never
substitutes another runtime or trusts a du result from the running guest.
Ordinary verified runsc measurement keeps its existing fresh-helper behavior,
with pinned commands and bounded helper/cleanup timeouts. The general helper
prerequisite is not built yet.
Docker's own volumes inside an ephemeral Kata guest remain available within the
main guest's owned resource boundary.

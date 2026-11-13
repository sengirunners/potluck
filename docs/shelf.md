# The package shelf

The shelf is a read-only tree of public package tarballs inside the `potluck-cache` volume, mounted at
`/opt/potluck-cache` in a box. Only the host writes it, and only with content it fetched from the public
registry and whose hash it checked. A box has the shelf when describe lists capability `shelf` and not
otherwise: each create checks the volume just before it runs, and a create that cannot tell whether the volume
exists fails instead of making a box without it; a warm box is checked again when it is taken, and warm boxes
made for the other state are replaced when the volume appears or goes. Only a volume made or removed in the
moment between a create's check and the create itself escapes this, and a persisted box keeps the mounts it was
made with across stop and start. This page covers the npm and Go parts as a
box sees them, how the owner fills them, by hand or from what boxes ask for, and how the shelf is kept within
its limits.

A box can also be given a whole built `node_modules` instead of installing from the shelf: a dependency layer
(`potluck sandbox create --deps npm`, `docs/daemon.md`, Dependency layers). A layer's build installs from this
shelf, so the shelf makes a build fast and a layer makes the next sandbox's install go away.

## Turning it on, and looking at it

```sh
potluck host --shelf-gb 50                 # make the volume, bind the cap on every oci size
potluck host --shelf-fill boxes            # also fill from what boxes' lockfiles name (default: seed)
potluck host shelf fill --npm-lock package-lock.json
potluck host shelf status
```

`--shelf-gb`, `--shelf-fill` and `--volume-max-gb` are `potluck host`'s flags for the keys `shelf_max_gb`,
`shelf_fill` and `volume_max_gb` ([hosts.md](hosts.md), "The package shelf"). They bind the same value on every
size the run binds (`shelf_max_gb` and `shelf_fill` must match across the engine; `volume_max_gb` is each size's
own). The default shelf fill is `seed`, so box fill is on only when the owner passed `--shelf-fill boxes` (a re-run
keeps it). `potluck host shelf status` prints the shelf's path, bytes and files (never a name), the fill
mode, the limits and the queue, and warns where sizes of one engine carry different
`shelf_max_gb`, `shelf_fill` or `shelf_keep_s`.

The shelf changes nothing on the network ([network.md](network.md)). What a box can learn from it: whether a blob
is on the shelf, so that someone on the same host recently used a given public package. That is the owner's
accepted cost of sharing one shelf; it names no one and no private package.

## Layout

```
/opt/potluck-cache/npm/content-v2/sha512/<aa>/<bb>/<rest>   one npm tarball, mode 0444
/opt/potluck-cache/goproxy/<module>/@v/<version>.<ext>       a GOPROXY tree: .info, .mod, .zip, .ziphash, mode 0444
/opt/potluck-cache/tools/go/<version>/<arch>                 Go toolchains (scripts/box-cache-fill.sh --go)
/opt/potluck-cache/bin/potluck-deps                          the in-box helper
```

`<aa><bb><rest>` is the tarball's sha512 in hex, split the way npm splits paths in its own cache
(`_cacache/content-v2/sha512/...`). The path is the hash, so a file there is a whole tarball. The shelf
has no index and records nothing about who asked for what.

## How a box uses it

```sh
/opt/potluck-cache/bin/potluck-deps link [dir]   # dir defaults to .
npm ci                                           # or npm ci --offline
```

The driver's own checkout runs the link step for you. A workflow or script that does not use it (a job that
checks out for itself, a sandbox) adds one line before its install; `;` and not `&&`, so a box without the shelf
(no such file) goes on to install as it always did:

```sh
/opt/potluck-cache/bin/potluck-deps link . ; npm ci
```

`potluck-deps link` reads `dir/npm-shrinkwrap.json` or `dir/package-lock.json` (lockfile versions 1, 2
and 3, with workspaces; run it in the directory that holds the lockfile, the workspace root). For each
package whose `resolved` URL is a registry tarball, `https://registry.npmjs.org/<name>/-/<base>-<version>.tgz`,
and whose `integrity` has a sha512, it does two things:

- If the shelf has that blob, it makes a symlink to it at the same path in the box's own npm cache
  (`$npm_config_cache`, otherwise `npm config get cache` run in `dir`, otherwise `~/.npm`). It leaves alone
  anything already at that path.
- Whether or not the shelf has it, it appends `npm <name> <version> sha512-<base64>` to
  `$HOME/.potluck/wants`, unless that line is already there. Name and version come from the URL, so an
  npm alias is named by the package it fetches. With `shelf_fill=boxes` the host reads this list to fill
  the shelf (below).

Packages from any other registry, `git`, `file` and workspace `link` entries, and entries with only a sha1
are left out of both. The helper exits 0 without doing anything when the shelf is not mounted, there is no
lockfile, or `node` is missing. `POTLUCK_SHELF` overrides the shelf root (tests use it).

The box never has to trust the shelf. npm looks a tarball up by the sha512 from the box's own lockfile and
checks the bytes against that hash on every read. A missing blob is a cache miss; a wrong one is treated
as a corrupt cache entry: npm removes the symlink (in the box's own cache; the shelf is read-only), then
fetches the package from the registry, or fails under `--offline`.

What this does not protect. The helper's filtering serves the box; it is not a boundary for the host. A
box can write any lines it likes to its wants file, so the host validates every line and fetches only from
the public registry, anonymously. A package that is private but hosted on npmjs.org is named in wants
(the host's anonymous fetch fails and stores nothing). And any box can see which blobs are on the shelf,
so it can tell that someone on the same host asked for a given public package within the keep time.

`npm ci --prefer-online` skips the cache and downloads everything, replacing the symlinks with the
downloaded files. Use plain `npm ci`, `--prefer-offline` or `--offline`.

## Go modules

Every box with the shelf is created with

```
GOPROXY=file:///opt/potluck-cache/goproxy,https://proxy.golang.org,direct
```

so `go build`, `go mod download` and the rest read a module from the shelf when it is there and fetch it as
Go always does when it is not. This is set on the container, so sandboxes, job runs, runners and warm boxes
all get it. What wins over it:

- an image whose own environment sets `GOPROXY` keeps its value (the driver checks the image's config at
  create and then sets nothing);
- a `GOPROXY` the run sets in its environment (`potluck run --env`, a workflow's `env:`, `export`) wins, as
  any environment variable does;
- `GOPRIVATE` and `GONOPROXY` keep their meaning: Go fetches the modules they match directly, never through
  any proxy, so they never read from the shelf.

A `GOPROXY` set with `go env -w` loses to the container's variable, as Go always ranks its environment above
its go env file; set it in the environment instead.

Go checks every module it reads against the box's own `go.sum` (and the checksum database for one `go.sum`
lacks), so the box never has to trust the shelf: a wrong file fails the build with Go's checksum error, never
passes as the module (unless the box turned the checks off). A module in `go.sum` and on the shelf builds with
no network. The shelf keeps no version lists (`@v/list`; the publisher removes those an older fill left), so
`@latest` and `go get -u` fall through to the public proxy.

A run that calls `potluck-deps link` after its build, with the Go settings it built with in its
environment, also has it read `go.sum` (and `go.work.sum`) in the directory and append to
`$HOME/.potluck/wants`, once each, `go <module> <version>` for each module version and
`go <module> <version>/go.mod` for each go.mod used alone (not for a version already named whole), but only
versions the box's own module cache holds (`GOMODCACHE`'s `cache/download`: Go downloaded them in this box;
a vendored module or a line written on another machine is not named; a go.mod alone needs only its `.mod`).
And it names only modules the box's settings send to the public proxy first:

- none at all unless the first network entry of the box's `GOPROXY` is `https://proxy.golang.org` (a single
  `file://` tree, like the shelf's, may come before it) and its `GOSUMDB` is `sum.golang.org`;
- none that the box's `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB` or `GOINSECURE` match;
- none at all when one of those holds a pattern other than plain globs (`*`, `?`): a `[class]` or a `\`
  escape names nothing, rather than risk naming a private module.

So a module it names is one the box's own Go asked proxy.golang.org for before anything else: the host's
fetch tells the proxy nothing the box had not. A private module the box fetched with `direct` (the proxy
had nothing, and the box had git credentials) without marking it private can be named; the host keeps its
name only in its queue, and asks the proxy again, which still has nothing. Mark private modules private.

It reads these settings with `go env` (the `go` on `PATH`, else the shelf's newest toolchain), so a go env
file counts. Settings the build had but the helper's environment lacks (a `GOPRIVATE` set for one command)
it cannot see: run it with the same environment. With no `go` at all it reads the environment, and names
nothing if a go env file exists. It reads
`go.sum`, not `go.mod`: a `go.mod` can name a module replaced by a local directory, which was never fetched.
The driver's own link step after a checkout names no Go module (`POTLUCK_DEPS_GO=off`): it runs before the
job's script, which may set `GOPRIVATE` itself.

## Filling it from a lockfile

```sh
potluck host shelf fill --npm-lock package-lock.json [--npm-lock other/package-lock.json ...]
```

It runs on each `vm=oci` size this machine shares with `shelf_max_gb` above 0 (it does nothing when that is
0 or `shelf_fill` is `off`), through that size's driver (`potluck-driver-tart host shelf fill`), which must
already be pinned (`potluck driver pin tart`, or the daemon's first use). A fill stages no tarball that
would take the shelf past `shelf_max_gb` (dropped as `full`) and fetches nothing while the host is below
its free-disk floor (dropped as `floor`; see Limits and eviction below). The
lockfile is read the way `potluck-deps` reads it: only entries resolved from `https://registry.npmjs.org/`
with a sha512 integrity count. For each one not already on the shelf, the host:

1. asks `https://registry.npmjs.org/<name>/<version>` (a scoped name's slash written `%2f`), with no
   credentials, no proxy, and no redirects followed;
2. drops the package unless the answer names the same package and version and its `dist.integrity` holds
   the lockfile's sha512;
3. drops it unless `dist.tarball` is on the same scheme and host;
4. downloads the tarball at most 256 MiB, paced to `shelf_fill_mbit` (default 16), and keeps it only if its
   sha512 is the lockfile's; then the publisher puts it on the shelf.

A dropped package (404, 401, a redirect, a different hash, too big, another host) is counted by reason and
forgotten: nothing is stored, and its name is not printed.

```sh
potluck host shelf fill --go-sum go.sum [--go-sum other/go.sum ...]
```

reads each `go.sum`, leaving out what `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB` or `GOINSECURE` match on this
machine, and refuses unless this machine's `GOPROXY` goes first to the public proxy, its `GOSUMDB` is
`sum.golang.org` and its patterns are plain globs (as the helper names nothing then). Settings come from
`go env` (without a go, the environment; it refuses when a go env file
exists). For Go modules not on the shelf, the host:

1. starts a throwaway container of the size's own pinned image, under its runtime, on its box network (so the
   owner's egress filter and the bandwidth cap apply), as user 65534 with every capability dropped and the
   shelf read-only, for at most 8 versions at a time; the fill's cap, the room under `shelf_max_gb` and the
   free-disk floor are checked before each;
2. in it, runs the shelf's newest Go toolchain for the machine's arch (`tools/go/<version>/<arch>`, added with
   `scripts/box-cache-fill.sh --go`; none, and the modules are dropped as `toolchain`) with
   `GOPROXY=https://proxy.golang.org` (never `direct`, so the host never contacts a server a box named),
   `GOSUMDB=sum.golang.org`, nothing private, no go env file and `GOTOOLCHAIN=local`: `go mod download` for a
   version, `go list -m` for a go.mod alone. Go checks each download against the checksum database before
   it keeps it; Go's own output, which names modules, is dropped;
3. takes back only the `.info`, `.mod`, `.zip` and `.ziphash` files of the versions it asked for, under size
   caps, and keeps a version only when all its files came, its `.info` names it, and its `.zip`'s Go hash is
   its `.ziphash`;
4. publishes: the publisher checks the same again and puts each file on the shelf 0444, every `.zip` first and
   each `.info` last. A version counts as on the shelf when its `.info` and its `.zip` (or, for a go.mod alone,
   its `.mod`) are.

A binding whose boxes have no network drops Go modules as `network`; one the proxy or the checksum database
has nothing for is dropped as `missing`. The image needs `sh`, `tar`, `find`, `sort`, `cut`, `tail`, `uname`,
`mktemp` and CA certificates.

## Filling it from boxes' wants

With `shelf_max_gb` above 0 and `shelf_fill=boxes` (the default is `seed`: only the owner's fills), the
host collects what boxes ask for:

- A box acquired with a commit (the driver's own checkout) gets `potluck-deps link <checkout>` run in it
  right after the checkout, as the box's user, for at most 30 s, whatever `shelf_fill` is. A failure goes to
  the run log and the box carries on. Other boxes (sandboxes, runner jobs) run the helper themselves.
- The host reads the box's `$HOME/.potluck/wants` by running `cat` inside the box (at most 4 MiB, for at most
  5 s): after that link step, and once more as the box ends (release, expiry, or its run's end), while it still
  runs. The path is the box's own `$HOME`, read through the box; the host never reads a wants file from its
  own disk, so on a machine where a job's `$HOME/.potluck` is the daemon's home nothing is read from there.
- Each line must be a valid want (`npm <name> <version> sha512-<base64>` or `go <module> <version>[/go.mod]`
  with a module path Go accepts and a canonical version, at most 512 bytes); other lines are dropped. One box queues at most 20,000 wants, each once; the whole queue holds at most 64 MiB on the
  host. The queue is in the host's state directory (`shelf/queue/`), never in the volume.
- A filler process, at most one per state directory, fetches them as the owner's fill does (above). A box
  can write any line, so a private module it names on purpose is asked of the public proxy, anonymously,
  which has nothing and stores nothing. A want
  from a live box waits until the box ends or `shelf_fill_delay_s` (default 300) passes, so the host does not
  download beside a box downloading the same thing. One box's wants fetch at most 2 GiB of new tarballs;
  the rest are dropped. A want already on the shelf costs no download.

Logs count wants by outcome and never name a package or a module. Switching `shelf_fill` away from `boxes` drops the
queue at the driver's next call. What a box learns is unchanged: whether a blob is on the shelf.

## Limits and eviction

A blob's last use is its mtime; a Go module version's is its `.info`'s (else its newest file's). The
publisher sets it when it puts the blob or version on the shelf, and moves it to now when a fill (the
owner's or from boxes' wants) names one that is already there, at most once a day each (or once per half
`shelf_keep_s`, when that is shorter).

After every publish, and when the filler starts (the driver's sweep starts one at least every 6 hours
while `shelf_max_gb` is above 0, whatever `shelf_fill` is), the host lists the volume's files, sizes and
mtimes through a read-only helper container, then deletes, through the publisher's read-write container:

1. every npm blob and Go module version last used before `shelf_keep_s` (default 30 days);
2. then the oldest of them until the whole volume's files total at most `shelf_max_gb` (sizes, not disk
   blocks). When the other files alone reach `shelf_max_gb`, this step deletes nothing and nothing fills.

Only npm blobs (`npm/content-v2/sha512/...`) and whole Go module versions are ever deleted. A version's
files go together, its `.info` first, so a box never finds a version whose other files are going (it reads
a missing file as a miss and fetches the module as Go always does). `bin/`, `tools/` and the rest of
`goproxy/` count toward the size but are never touched by eviction: `tools/` is the owner's. (The next
publish removes the `list`, `.lock` and `sumdb/` files an older fill script left in `goproxy/`.) A blob or version whose mtime changed between the listing and the delete
(touched, or published again by another size sharing the volume) is left.

Filling stops where the limits are:

- A tarball or Go module version that would take the shelf past `shelf_max_gb` (counting what is waiting
  in the host's staging) is dropped (`full`); a tarball before it downloads when the registry sends its
  length, a Go version after the fetch container brought it, so it costs the download once. Blobs still within their
  keep time are never pushed out to make room, so a box asking for many packages cannot evict what other
  boxes use; room comes back as blobs pass their keep time. The other side of that: what fills the shelf
  and keeps being asked for holds its room.
- Below the host's free-disk floor `persist_min_free_gb` (under the engine's storage directory or the
  driver's state directory) nothing is fetched (`floor`) and nothing is published.

Every `vm=oci` size on one engine shares the one `potluck-cache` volume, and each applies its own
`shelf_max_gb` and `shelf_keep_s` to all of it: the smallest of each wins, and a size with a larger cap
refetches what a smaller one evicts. Give every size on an engine the same values.

Only fills renew a blob. With `shelf_fill=seed` (the default) boxes' wants are not read, so a seeded blob
goes `shelf_keep_s` after the last seed that named it, however often boxes use it; seed again, or use
`shelf_fill=boxes`.

A deleted blob is unlinked, never truncated: a box reading it keeps reading the whole file, and a box's
cache link to it dangles. npm treats a dangling link as a cache miss: plain `npm ci` downloads that package
from the registry, `--offline` fails with `ENOTCACHED` (`TestNpmCiRefetchesDanglingLink` in
`vmhost/shelfhelper`).

## Why symlinks and no index entries are enough

npm's cache (cacache) has two parts: `index-v5` maps request URLs to content hashes, and `content-v2` holds
the content by hash. For a tarball whose lockfile entry has an `integrity`, npm (pacote) reads the content
by that hash and does not consult the index. So the box's cache needs only the content files, and a
symlink to a read-only file is read like the file.

Measured on 2026-11-12 with npm 11.20.0 (Node 26) and npm 10.9.2 (the npm in Node 22), same results. A
project with `chalk@4.1.2`, `semver@7.6.3` and `debug@4.3.7` (9 packages) was installed once into a
throwaway cache; its content files were copied to a shelf directory (files 0444, directories 0555). Then,
for each run, a new empty cache was given only symlinks to the 9 tarball blobs, as `potluck-deps link`
makes them (no other files), `node_modules` was removed, and `npm ci` ran with `--loglevel=http`:

| run | result |
| --- | --- |
| `npm ci --offline` | `added 9 packages`, all 9 `(cache hit)`, exit 0 |
| `npm ci` | `added 9 packages`, all 9 `(cache hit)`, exit 0 |
| `npm ci --prefer-offline` | `added 9 packages`, all 9 `(cache hit)`, exit 0 |
| `npm ci --prefer-online` | `added 9 packages`, all 9 `(cache miss)` fetched from the registry, links replaced by files, exit 0 |
| `--offline`, `ms` not on the shelf | `ENOTCACHED ... cache mode is 'only-if-cached'`, exit 1 |
| plain, `ms` not on the shelf | 8 hits, `ms` fetched `(cache miss)`, exit 0 |
| plain, `ms`'s blob is junk bytes | `TAR_BAD_ARCHIVE`, `cached data for ms@... seems to be corrupted. Refreshing cache.`, refetched, exit 0 |
| plain, `ms`'s blob is another package's valid tarball | `seems to be corrupted. Refreshing cache.`, refetched, installed `ms`, exit 0 |
| `--offline`, either bad blob | the same warning, then `ENOTCACHED`, exit 1 |

In every run the shelf files were unchanged afterwards. `TestNpmCiOfflineFromLinks` in
`vmhost/shelfhelper` repeats the offline case without a network, with whatever npm is on `PATH`.

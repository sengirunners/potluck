# Push bases

A supporting host keeps a sealed Git snapshot for each holder, project,
repository history and UTC ISO week. `run`, `agent-task`, and sandbox creation
with a local push request it automatically. A later push discovers the mounted
base and sends only the objects its commit needs beyond that snapshot.

The first request on a host fills a base in a separate networkless builder box.
The builder has no task credential, dependency layers or caller volumes. Its
lease is at most 15 minutes and cannot be renewed. Release seals only `objects`
and `refs`; an abandoned builder is discarded. Concurrent builders wait for up
to three minutes before falling back to a whole push.

The base volume is mounted read-only at `.potluck-base` under the box user's
home. The existing sealed-volume quota and eviction rules apply. A new UTC
week selects a new volume; existing boxes keep their original mount. The base
is an optimization: an unsupported host runs without it, and a failed fill or
an evicted base falls back to a whole push.

## What the box can read

A base belongs to one holder and project on one binding. Repository identity
is a hash of its root commits, not a URL; forks with the same roots therefore
share a base for that holder and project. Different holders and projects never
share these private snapshots.

A box can read the snapshot used to fill its base, including files since
deleted from the repository. The base commit is a common ancestor with a
locally recorded remote default branch, or the requested commit if there is
no such ancestor. No remote fetch occurs during identification. Shallow
checkouts do not request a base because they cannot identify their real roots.

Use `--no-base` when creating the sandbox or running the task to avoid mounting
one. `--full` also opts out. On a push into an existing sandbox, `--no-base`
disables discovery; it cannot remove a volume already mounted in that sandbox.
The MCP sandbox creation tool accepts `no_base` for the same opt-out.

Only the chosen snapshot is seeded, not all local refs or intermediate
history. Pushes retain commit and object verification, annotated-tag handling,
and fallback when base objects are incomplete. The base's Git configuration
and hooks are never loaded during a push.

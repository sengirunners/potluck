# Project database snapshots

Use a project-owned recipe from any box command:

```sh
potluck sandbox create --database example-db
potluck run --database example-db --deps npm -- npm test
potluck agent-task --database example-db --deps npm --prompt-file task.md
```

`--database` selects the recipe's image, so it cannot be combined with
`--image`, `--template` or `--persist`. An agent task keeps its credential and
proxy requirements, but does not select the default agent template when a
database recipe supplies the image. That image must include the selected agent.
The automatic Git base and disjoint dependency layers remain available.

A box acquire can name a database recipe with `"database":"example-db"`. An
owner or admin of the whole project sets that name, live, on the project's
daemon (through the pool with `-P <pool>`):

```sh
potluck sandbox database set example-db \
  --image registry.example/team/example-db@sha256:<64 lowercase hex digits> \
  --manifest-file manifest.json            # the image's manifest.json, byte for byte; - reads stdin
potluck sandbox database                   # name and image of each; --json adds the manifest
potluck sandbox database rm example-db
```

The daemon checks the recipe with the same rules as below and rewrites
`project.toml`; the very next acquire naming it resolves the new recipe, with
no daemon restart. Setting the recipe the project already has writes nothing
(`"changed": false`). `--if-image <old pin>` sets it only while the recipe still
has that image, else answers a conflict, so a job that read the old pin never
overwrites a newer one. The HTTP routes are `GET /v0/databases` (read on the
project) and `PUT` / `DELETE /v0/databases/{name}` (body `image`, `manifest`,
optional `if_image`). In `project.toml` a recipe reads:

```toml
[[databases]]
name = "example-db"
image = "registry.example/team/example-db@sha256:<64 lowercase hex digits>"
manifest = "<the exact JSON text of the image's manifest.json, TOML-escaped>"
```

An edit of `project.toml` by hand is read only when the daemon starts, and a
`set` after one is refused until it restarts.

The image must also be lent by the chosen box binding, pinned to that same
immutable digest. A binding that does not lend it refuses before it makes
anything, so placement asks the next database-capable binding. Its `/opt/potluck-db/manifest.json` and `pgdata.tar.gz` follow
`potluck-postgres-snapshot-v1`. It contains the PostgreSQL runtime under `/nix`
and `/usr/lib/postgresql`, and its configuration under the roots its manifest's
`config_archives` names: `/etc/postgresql`, and siblings such as Supabase's
`/etc/postgresql-custom`. Each entry maps a root to the archive the producer
built it from and that archive's sha256:

```json
"config_archives": {
  "/etc/postgresql": {"archive": "postgres-config.tar.gz", "sha256": "<64 hex>"},
  "/etc/postgresql-custom": {"archive": "postgres-custom-config.tar.gz", "sha256": "<64 hex>"}
}
```

Every file in `config_files` must lie under `pgdata_path` or one of these roots.
`manifest.project` identifies the source repository; the recipe belongs to the
project containing this configuration. A pool project can therefore lend an
owner-approved snapshot of a different source repository.

Only trusted hosts and OCI box drivers advertising `database-snapshot-v1` can
serve these requests. There is no fallback to an ordinary box or dependency
layer. Persistent boxes, runner configs and image/template overrides cannot
be combined with a database request. Database acquires never take an ordinary
warm box.

Push bases and dependency layers compose with the database when their mount
paths are disjoint. Dependency cache keys and builders use the exact resolved
database image; builders do not start Postgres. Repeated tasks reuse the sealed
Git base and dependency layers. No volume or dependency mount may overlap
`~/.potluck-database`, and volume mounts cannot cover `/opt/potluck-db`.

The driver makes a fresh private copy of the complete, cleanly stopped PGDATA.
It checks the exact manifest and snapshot checksums, architecture, control file,
server version, migration ledger and extension versions before reporting ready.
The ledger must match the manifest's `migration_count` and
`migration_versions_sha256`, not merely the count. Each configuration root is copied into the private guest directory
(`/etc/postgresql` to `config`, `/etc/postgresql-custom` to `config-custom`),
and the configuration sources' references to PGDATA and to those roots are
rewritten to the private copies, whole path names only (`/opt/etc/postgresql`
is left alone).
Postgres listens only on guest loopback, with a random password and no published
port. The image must supply Python 3.12 or newer and the runtime identified by
the manifest. A missing prerequisite refuses the acquire.

The snapshot follows the Supabase CLI's migration layout, though no Supabase
service runs:

- The manifest's `extensions` must list `pgtap`, and must equal the snapshot's
  `pg_extension` versions exactly.
- The migration ledger is read from `supabase_migrations.schema_migrations`.
  `migration_versions_sha256` is the sha256 of its versions in byte order, each
  followed by a newline (`printf '%s\n' <versions> | LC_ALL=C sort | sha256sum`),
  and `migration_count` is how many there are. The manifest therefore stays the
  same size however many migrations a project has; `inputs_sha256` identifies the
  producer's inputs, which the manifest need not list.
- The postgres role's random password is set as the cluster's bootstrap
  superuser (Supabase's `postgres` role is not a superuser).
- The box's guest user must not be root; the bootstrap refuses uid 0.
- The manifest is at most 256 KiB, because the driver echoes the recipe in each
  acquire answer and a remote host's answer has a size cap. A manifest must
  not grow with the project: list no per-file inventory in it.

The daemon refuses a recipe whose manifest breaks a rule it can check itself, naming that rule.

Every exec, background process and terminal receives `DATABASE_URL` and its `DB_URL` alias. The URL
stays in the guest's private directory and is never returned as handle metadata,
stored in driver state, or put in host command arguments. This is a database
only: it provides no Supabase API, authentication service or object storage.
Release, cancellation, failed startup and lease expiry use the box's normal
lifecycle and destroy its private database along with the box.

An acquire's durable identity includes the exact recipe image and manifest.
Repeating its request ID keeps that resolved recipe even after configuration
changes; changing the recipe name conflicts. The daemon releases a driver
answer that does not confirm the exact requested provenance or image.

Recipes authorize lending the configured snapshot to that project's box users.
They do not make an otherwise shared image private: anyone who can acquire the
same image directly can read its baked snapshot. Restrict the binding's image
lending to the intended project and users. Never bake production data, credentials
or user content into these images; use schema and approved seed data only.

This capability does not publish images, select a registry, or update the recipe
when origin/main changes. The artifact producer and project owner supply those
immutable inputs. An image publication and real-box SQL check remain deployment
steps, separate from installing this driver capability.

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

A box acquire can name a database recipe with `"database":"example-db"`. The
project owner configures that name in `project.toml`:

```toml
[[databases]]
name = "example-db"
image = "registry.example/team/example-db@sha256:<64 lowercase hex digits>"
manifest = "<the exact JSON text of the image's manifest.json, TOML-escaped>"
```

The image must also be lent by the chosen box binding, pinned to that same
immutable digest. A binding that does not lend it refuses before it makes
anything, so placement asks the next database-capable binding. Its `/opt/potluck-db/manifest.json` and `pgdata.tar.gz` follow
`potluck-postgres-snapshot-v1`. It contains the PostgreSQL runtime under `/nix`
and `/usr/lib/postgresql`, and its configuration under `/etc/postgresql`.
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
The ledger must match the migration filenames in the manifest's inputs, not
merely their count. Configuration is relocated into the private guest directory.
Postgres listens only on guest loopback, with a random password and no published
port. The image must supply Python 3.12 or newer and the runtime identified by
the manifest. A missing prerequisite refuses the acquire.

The snapshot follows the Supabase CLI's migration layout, though no Supabase
service runs:

- The manifest's `extensions` must list `pgtap`, and must equal the snapshot's
  `pg_extension` versions exactly.
- The migration ledger is read from `supabase_migrations.schema_migrations`. Its
  versions must equal the filename prefixes (before the first `_`) of the
  manifest's `supabase/migrations/*.sql` inputs; other inputs do not count.
- The box's guest user must not be root; the bootstrap refuses uid 0.
- The manifest is at most 256 KiB, because the driver echoes the recipe in each
  acquire answer and a remote host's answer has a size cap.

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

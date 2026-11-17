# Coming from zm?

Potluck began as code in zm, an earlier Sengi tool. If you never used zm, you can skip this page: nothing here is
needed to run Potluck.

If you did run it as zm, most of it keeps working. Potluck sends and creates its own names and still reads the old
ones (with the exceptions at the end):

- Programs: `potluck-driver-tart` and `potluck-driver-local` were `zm-run-tart` and `zm-run-local`. The daemon looks
  for the new name first, then the old one, and the source still builds the old names (`cmd/zm-run-tart` and the like)
  for build lists that name them. A release ships only the new names.
- Home and state: Potluck's home is `$POTLUCK_HOME` (default `~/.potluck`). On a machine whose running daemon is
  another program's built on Potluck, and with no `~/.potluck`, potluck's commands use that daemon over its socket
  ([daemon.md](daemon.md#one-daemon-per-machine)). Potluck never opens that program's home's database and never
  starts a daemon on it; with a home of its own, potluck runs its own daemon, and nothing of the other home is
  carried over. A driver's state directory `~/.zm-run-<driver>` is kept where it exists.
- Networks and units on a Linux host: `zm-box`, `br-zmbox` and `zm-box-net.service` are read and capped until you
  move to `potluck-box` ([network.md](network.md)).
- Box images and labels: VM names `zmrt-<hash>-`, and labels `dev.zm.*`, are still swept and counted.
- Tailnet nodes, tokens and headers: existing nodes keep their `zm-<machine>` names; tokens `zm_...` and headers
  `Zm-*` are still accepted.

The full table, with what each side sends and reads, is in [protocol.md](protocol.md#names).

Commands: `potluck sandbox` still runs zm's old spellings that programs call, hidden from `--help`, each the new
command with the same flags and the same `--json` answer:

| old | now |
| --- | --- |
| `sandbox spawn <sandbox> -- <argv...>` | `sandbox exec <sandbox> --detach -- <argv...>` |
| `sandbox output <sandbox> <pid>` | `sandbox logs <sandbox> <pid>` |
| `sandbox kill <sandbox> <pid>` | `sandbox signal <sandbox> <pid>` |
| `sandbox renew <sandbox>` | `sandbox extend <sandbox>` |
| `sandbox cp <local> box:<sandbox>:<path>` (and the other way) | `sandbox cp <sandbox> <local> :<path>` |

`potluck host` runs two more the same way:

| old | now |
| --- | --- |
| `host share [--pool <pool>] ...` | `host [<pool>] ...` (the pool is host's argument; every host flag) |
| `host doctor` | `doctor` (its `--json` answer exits 0 whatever doctor found, as before) |

zm's `lend pause` and `lend resume` are `potluck host pause` and `host resume`: a pause is the host switch closed,
so `host status`, the menu bar item and every driver see it; nothing is unshared. zm's `lend switch` is
`host set` (which takes `close` for `closed`), `lend stop` is `host stop`, and `lend --when` and `--detach` are
`host --when` and `--detach`.

`potluck run` runs the commit's `.zm/job` when it has no `.potluck/job`.

Not read any more: `dev.zm.volume=1` volume labels and `$HOME/.zm/push` chunks (a resumed push sends them again).

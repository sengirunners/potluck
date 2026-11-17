# Coming from zm?

Potluck began as code in zm, an earlier Sengi tool. If you never used zm, you can skip this page: nothing here is
needed to run Potluck.

If you did run it as zm, most of it keeps working. Potluck sends and creates its own names and still reads the old
ones (with the exceptions at the end):

- Programs: `potluck-driver-tart` and `potluck-driver-local` were `zm-run-tart` and `zm-run-local`. The daemon looks
  for the new name first, then the old one, and the source still builds the old names (`cmd/zm-run-tart` and the like)
  for build lists that name them. A release ships only the new names.
- Home and state: Potluck's home is `$POTLUCK_HOME` (default `~/.potluck`). It does not read zm's `$ZM_HOME`, so
  zm's pools and projects are not carried over. A driver's state directory `~/.zm-run-<driver>` is kept where it
  exists.
- Networks and units on a Linux host: `zm-box`, `br-zmbox` and `zm-box-net.service` are read and capped until you
  move to `potluck-box` ([network.md](network.md)).
- Box images and labels: VM names `zmrt-<hash>-`, and labels `dev.zm.*`, are still swept and counted.
- Tailnet nodes, tokens and headers: existing nodes keep their `zm-<machine>` names; tokens `zm_...` and headers
  `Zm-*` are still accepted.

The full table, with what each side sends and reads, is in [protocol.md](protocol.md#names).

Not read any more: `dev.zm.volume=1` volume labels and `$HOME/.zm/push` chunks (a resumed push sends them again).

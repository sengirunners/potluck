# Stranger test

What a newcomer hits on a clean machine when they follow only the public instructions (the README quick start
and potluck.sh) to a sandbox that answers `uname -a`. It is not a test that the path works; it finds where a
person slows down: installing Tart and Softnet, the sudo line, disk space, the first image pull, unclear steps.

```sh
scripts/stranger-test/run.sh --platform mac   --mode agent      # a fresh-context agent on a clean Mac home
scripts/stranger-test/run.sh --platform linux --mode scripted   # the README steps, as written, in a fresh Ubuntu VM
scripts/stranger-test/run.sh --cleanup <run dir>                # stop and delete what a run made; keeps its logs
```

It runs on an Apple-silicon Mac with Tart and `claude` (for agent mode). Each run gets its own directory, by
default under `~/.cache/potluck-stranger/runs/` (`--out` must name a new or empty one). Recordings stay out of
git. Before it starts, a run refuses when the disk it lives on has less free space than it needs (70 GB for a
Mac run, 25 GB for a Linux run; `STRANGER_MIN_FREE_GB`), or, for a Mac run, when two VMs already run on this Mac
(macOS allows two macOS VMs at once; the count includes Linux VMs, so it refuses early rather than late).

## The verdict

`report.md` starts with PASS or FAIL, and `run.sh` exits 0 only on PASS. The harness decides from `nc`'s own record,
not from the agent's reply: PASS needs a step whose whole command is `potluck sandbox exec <id> -- uname -a`, run
on the clean machine through `nc`, that exited 0 and printed a first line starting `Linux ` or `Darwin `, where
`<id>` was listed as ready by an earlier `potluck sandbox create` step. FAIL names the last step reached. This
stops honest mistakes (a host's `uname`, a missing sandbox, an error with exit 0), not a determined cheat: an
agent could still define a shell function named `potluck` on the clean machine. Its transcript shows that. The report also lists
every step the harness stood in for (`SUBSTITUTED` lines in `terminal.log`), and, for Linux, that the stock
image's sudo needs no password.

## The clean machine

- **mac**: a fresh `HOME`, `TART_HOME` and `PATH` (`~/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin`) on the Mac that
  runs the test. Nothing in `/usr/local`, `/opt/homebrew` or the real home is found, Potluck's daemon records live
  under the fresh home, and the first macOS image pull is real. It is not a fresh Mac: the network, the login and
  anything already in `/usr/bin` are the host's. `machine-before.txt` lists what the host already had that a
  newcomer would not (Tart, a setuid Softnet, Docker, Homebrew, images). A macOS guest would be cleaner, but
  Apple silicon does not run a macOS VM inside a macOS VM. Because it is the test host, the run's `sudo` (first on
  its `PATH`) refuses every call: no password can be typed, and a plain `sudo` gets no root on this Mac even where
  a sudo ticket or NOPASSWD would allow it. `/usr/bin/sudo` by its full path goes around this, so on such a host a
  run could still get root; run agent mode where the user's sudo asks for a password. `STRANGER_ALLOW_SUDO=1`
  passes calls to `sudo -n` instead.
- **linux**: a fresh Ubuntu 24.04 VM (`ghcr.io/cirruslabs/ubuntu:latest`, arm64) cloned into the run's own
  `TART_HOME`, with a 60 GB disk. Commands reach it through `tart exec`. The stock image (pulled once into
  `~/.cache/potluck-stranger/linux-base-tart`) has passwordless sudo and no Docker. The farm cannot run this yet:
  `hub run` runs only in onCall checkouts, and `hub dispatch` runs an agent on a farm box, not a fresh VM.

## Two modes

- **agent**: a fresh `claude -p` session (Sonnet by default) with no CLAUDE.md, memory or repository: it starts
  in an empty directory with project-only settings, gets `newcomer.md` as its whole brief, may read the public
  pages, and runs commands only through the run's `nc` program. It writes every guess and stumble to
  `stumbles.md`. It gets none of this user's MCP servers (`--strict-mcp-config`), only the built-in tools Bash,
  WebFetch, WebSearch, Read, Write and Edit, and runs without prompts only `nc`, the web and its stumbles file.
  This is not a sandbox: `nc` runs as the user who started the run, so on a Mac run the clean shell can read
  anything on the host that user can (the fresh `HOME` and `PATH` only keep the newcomer from finding it by
  accident). The brief keeps the agent to the public pages; its transcript (`agent.jsonl`) shows whether it did.
- **scripted**: `scripted-mac.sh` or `scripted-linux.sh` run the README steps as written. Nothing in them can
  guess, so their stumbles are written by whoever reads the log. Where the README leaves a step to a third-party
  site (Tart, Softnet, Docker), the script does it the way that site says, and says so in a comment. The Mac script
  cannot type a sudo password: it runs the root step doctor prints (refused, as above), and when that step is the
  Softnet one and this host's `/usr/local/bin/softnet` is setuid root, links that in its place (`SUBSTITUTED` in
  the log and the report); otherwise the step is `BLOCKED`. It logs which Tart and Softnet release it downloaded
(`softnet --version` too), and the report lists them.

## What a run leaves

| file | what |
| --- | --- |
| `machine-before.txt` | the machine, and what the host already had |
| `timings.tsv` | one row per step: name, start, end, seconds, exit code, output file, command |
| `steps/<start>-<pid>-<step>.out` | each step's output |
| `terminal.log` | every command and its output, with UTC times |
| `terminal.rec` | the terminal, replayable: `script -p terminal.rec` (BSD `script -r`; asciinema is not used because a clean Mac has none) |
| `stumbles.md` | one entry per stumble (agent mode writes it; in scripted mode the reader does) |
| `report.md` | all of the above as one page (`run.sh --report <run dir>` rebuilds it) |
| `agent.jsonl`, `agent-prompt.md` | agent mode: the session's transcript and its brief |

`nc.sh` is the step runner (`nc --help`): `nc <step> '<command>'`, and `nc --bg <step> '<command>'` with
`nc --wait <step>` for steps longer than an agent's tool call (a macOS image pull); `--wait` exits 75 while the
step still runs, and the step's own code when it ends. Every step has a timeout (`STRANGER_STEP_TIMEOUT`, default
7200 s); a child that started its own session (a daemon) can outlive it, and `--cleanup` stops the run's daemon
by the pid in its `daemon.lock`, only while that pid is still a program from the run's home.

## Known limits

- The install step calls GitHub's API without a token, which allows 60 calls an hour per IP address. From an
  office or another shared address the limit can be spent before the run starts: the step then fails with 403,
  which is itself a finding a newcomer would hit. Run again after the reset (`curl -s https://api.github.com/rate_limit`).
- A run reaches the goal on a Mac only past the Softnet sudo step; agent mode stops there (no password).
- An agent reads the public README at github.com/sengirunners/potluck, which follows this repository's main
  after a delay. A line already corrected here (such as the one about running on a real Mac) can still show there
  and mislead a run; check the public copy before filing what an agent quotes from it.

## Filing what it finds

One bead per stumble, as a child of the stranger-test bead: the exact step, what happened, the log path, a
suggested fix. Search first; a stumble already filed gets a comment with the new run's log path.

# potluck try

`potluck try` is the one-machine trial in one command: from a fresh install to `uname -a` in a sandbox, with no
Tailscale and no second machine.

```sh
potluck try
```

## What it does

1. **The pool.** Unless this machine is already in it, it makes the pool `trial` (or the one `--pool` names), run by
   this machine with no tunnel: what `potluck create trial --via tunnel` makes. A pool like that serves this machine
   alone.
2. **Sharing this machine.** When no machine in the pool shares sandboxes yet, it runs `potluck host trial
   --reclaim-idle-s 0`, printed before it starts. That is the ordinary host run: it checks the machine, runs the root
   step with sudo at a terminal (it asks for your password once; on a Mac it installs Softnet), pulls the image, runs
   the security checks and shares each size that passes. `--reclaim-idle-s 0` lets sandboxes run while you use the
   machine; without it a Mac's sandbox waits until you have been idle for five minutes.
3. **A sandbox.** It makes one, as `potluck sandbox create` does, runs `uname -a` in it, prints the output and removes
   it. Its lease is ten minutes, so a try you stop part way does not leave it running.
4. **Next steps.** It prints them: your own sandbox, an invite for a friend, macOS guests on a Mac, and how to stop
   sharing.

The machine stays shared with the pool afterwards, so the next `potluck sandbox create` is quick. `potluck host stop`
stops sharing it. Running `potluck try` again skips the pool and the share and makes another sandbox.

## On a Mac: a small Linux image first

The Tart layer lends macOS by default, an image of about 27 GB that wants about 60 GB free. `potluck try` lends
Potluck's small Linux image instead (Debian, about 1 GB to pull), the `tart linux ... name=small-linux` line in
[`imagepins/images.pin`](../imagepins/images.pin); it passes `--with small-linux --root=false` to `potluck host`. The rule is
`hostprobe.ChooseFirstImage`: on the layer `potluck host` would choose, when that layer's default is a macOS guest and
the pin table has a published image named `small-linux` for it (ghcr.io/sengirunners/potluck-box-tart-linux, public).
A pin line that holds `-` instead (an image not yet published) makes `potluck try` say so and lend macOS.

Its sandboxes are Linux VMs under Tart and Softnet with the same checks as any other: no root (the image has no
`sudo`), no SSH, and the network filtered as for macOS boxes ([docs/isolation.md](isolation.md)). They serve no
GitHub Actions jobs: the image has no runner.

For macOS guests afterwards, the last lines of `potluck try` print the command, with the pinned macOS image:

```sh
potluck host trial --image ghcr.io/cirruslabs/macos-tahoe-base@sha256:<digest>
```

It replaces the Linux image this machine lends (a host run that names no image would refuse, since the earlier run
lent `--with small-linux`).

### The image

`cmd/potluck-driver-tart/images/tart-linux/build.sh` makes it on an Apple-silicon Mac with Tart (it boots one VM;
GitHub's hosted runners have no nested virtualization): Cirrus Labs' Debian image with `perl` and `git` added, `sudo`
and `openssh-server` removed, the `admin` password locked, and the image's floor lowered from Tart's Linux default (4
vCPU, 4 GiB, which no size fits twice in half of an 8-core Mac) to 1 vCPU and 2 GiB. It pushes the image and prints
it by digest, for the pin line.

## Flags

| flag | what |
| --- | --- |
| `--pool`, `-P` | the pool (default `trial`) |
| `--image name@sha256:<digest>` | lend this image instead, as `potluck host --image` does (repeat) |

`--json` is refused: run the steps (`potluck create`, `potluck host`, `potluck sandbox create`) with `--json` instead.

## In a program that mounts Potluck's commands

`try` is one of the mounted commands ([docs/mounting.md](mounting.md)); its words name the other commands through
the program's prefix.

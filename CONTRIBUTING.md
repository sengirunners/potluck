# Contributing to Potluck

Potluck aims to let people run CI jobs and coding-agent sandboxes in fresh VMs on machines they already own.

The project is early. Interfaces will change. The protocol is `potluck/1`; the names it had before (`zm/v0` and
the rest) are still read, as `docs/protocol.md` ("Names") says. This repository holds the `potluck` program, the
driver protocol, the VM host layer and the conformance suite.

## What is welcome now

- Bug reports.
- Fixes to the docs.
- Work on the VM host layer (`vmhost/`) and on drivers. To add a hypervisor, start with `docs/vm-layers.md`.

For anything bigger than a small fix, open an issue first so we can agree on the shape before you write it.

Drivers for other runtimes can live in their own repositories. A driver only has to speak the protocol in
`docs/protocol.md` and pass the conformance suite.

## Build and test

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .        # prints nothing when files are formatted
```

The conformance suite runs against the reference driver (`conformance/refdriver`) as part of
`go test ./conformance/...`. To check a driver of your own, build it and run:

```sh
go run ./cmd/potluck-conformance <driver> --kind run --ref <ref> --commit <sha> --script <name>
```

Run `potluck-conformance --help` for every flag. `docs/conformance.md` explains the groups and what a skip means.
The suite makes and ends real resources, so point it
at a throwaway setup.

Tests must not touch your real home directory or start real VMs. See `AGENTS.md` for the fakes.

## A good pull request

- One purpose.
- Tests for what changed.
- Says what it was tested on (OS, runtime, and which tests ran).

CI runs build, vet and test on every PR.

## Sign-off

Contributors outside the maintainers: sign every commit with `git commit -s`. That adds a `Signed-off-by` line and
certifies the statements in the [Developer Certificate of Origin](https://developercertificate.org/). There is no
CLA.

## Review

Maintainers merge. First response within 7 days.

## Security

Never report a vulnerability in a public issue. See `SECURITY.md`.

## CI on pull requests

A pull request from a fork runs its checks on GitHub's own runners, with a read-only token and no secrets, so it may
run slower than one from a branch in this repository. There is nothing for you to do. A fork runs its own copy of the
workflow files, so repository settings are what enforce this; `internal/workflowlint` only flags workflow changes
in this repository that would send a fork's run to our runners.

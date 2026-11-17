---
slug: run-a-coding-agent-in-a-sandbox
kind: new
engine: claude
title: "Run a coding agent in a sandbox on a spare Mac"
page_title: "Run a coding agent in a sandbox on a spare Mac | Potluck"
page_description: "A test post that uses every part of the Markdown subset: a coding agent's commands run in a throwaway sandbox on a pool machine."
target_searches:
  - "run a coding agent in a sandbox" # 320/month
  - "sandbox for coding agents on a mac"
data_sources:
  - "Test fixture; not a real post"
drafted: 2026-11-10
model: claude-opus-5-5
date: 2026-11-12
updated: 2026-11-15
summary: "Run the agent's commands with `potluck sandbox exec`, so they happen in a throwaway VM on a pool machine and not on your laptop."
---

An agent's commands can run in a **throwaway sandbox** on a machine you already own, so a runaway build never touches your laptop. It takes three commands: *create*, *exec* and *rm*.

## What you need

Install Potluck, then start a pool on one machine. The [README](https://github.com/sengirunners/potluck) lists what each machine needs:

- A Mac with Tart and Softnet, or a Linux box with Docker and Kata or gVisor
- No account: machines find each other over Tailscale
- A second machine is optional

## Run one command

Create a sandbox, run a command in it, remove it:

```sh
potluck sandbox create                  # prints the sandbox's id
potluck sandbox exec <id> -- make test
potluck sandbox rm <id>
```

The steps, in order:

1. Create the sandbox and note its id.
2. Run the agent's command with `exec`.
3. Remove the sandbox when the job is done.

### Sizes

A size is the shape of one sandbox, such as `c1m2` (1 core, 2 GB of RAM). Write \* for a literal asterisk, and snake_case_names stay as they are.

| Size | Cores | Memory |
| --- | ---: | :---: |
| `c1m2` | 1 | 2 GB |
| `c2m4` | 2 | 4 GB |

> A job gets a secret only when you grant it.
> It arrives as a file, never in the environment.

## Next steps

See the [home page](/), the [blog](/blog) and [how it works](/#how).

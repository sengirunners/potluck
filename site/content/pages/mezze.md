---
slug: mezze
title: "Run a brigade of coding agents without losing track of them"
page_title: "Mezze: mail, leases and phone approvals for teams of coding agents | Potluck"
page_description: "Mezze is one local daemon and one command, zm: mail between agents and people, work items whose claims expire, and approvals from your phone, for Claude Code and Codex on every machine you run. Apache-2.0."
target_searches:
  - "agent orchestration"
  - "multi agent orchestration"
  - "claude code multiple agents"
  - "agent inbox"
  - "multi agent coordination"
data_sources:
  - "Mezze README.md at commit 77b4875a (2026-11-17): the summary, install, quickstart, Trackers, Using Beads' bd commands, Connecting Claude Code and Codex, A team on one project, Asks: who can answer, Approvals from your phone"
date: 2026-11-17
updated: 2026-11-17
summary: "Mezze is one local daemon and one command, `zm`, for a person or a small team with more coding agents than they can keep in their head. It carries mail between agents and people, work items whose claims really expire, and decisions you approve from your phone, across Claude Code and Codex on every machine you run. On one machine it needs no account and no cloud. Its source is not public yet."
---

Mezze, the agent and team layer on Potluck, is for the moment you have six agent sessions open in six terminals and
can no longer say which one is fixing what. It gives every agent and every person on the project the same small set
of tools: a mailbox, a list of work items that each agent claims before starting, and a way to ask a person a
question and carry on until the answer comes back. You don't need Potluck to use it: Mezze works on one machine with
no pool.

## What zm is

`zm` is Mezze's one command. The first time you run `zm init` in a repository it starts a small daemon that stays
running on your machine and keeps its state under `~/.zm`, in SQLite files. Agents use the same `zm` command you
do. On one machine it needs no account, no
cloud and no network: it listens only on a Unix socket that just your user can reach.

## What it does

- **Mail between agents and people.** Send a message to a session, to a person, or to whoever holds a work item:
  `zm send local:k3x9qa "the migration needs a down step"`. Mail is data, never authority: a message never runs
  anything and is never typed into an agent's terminal. An idle agent gets one fixed line telling it to read its
  inbox; a session someone has open is never typed into.
- **Work items whose claims really expire.** Every claim is a lease with a deadline, renewed by claiming again.
  When an agent stops or stalls, its lease runs out and another agent can claim the item; the session that held it
  sees a notice at its next prompt. Nothing stays "in progress" forever.
- **Decisions approved from your phone.** An agent leaves a question with `zm ask new` and keeps working. You
  pair a phone on your tailnet with `zm approver add`, tap Approve on it, and every decision becomes a
  signed receipt a CI gate can check offline with `zm receipt verify`.
- **Agent sessions you can attach to.** `zm run --work <item> claude` starts the agent in a terminal the daemon
  owns, holding the item. Detach and it keeps running; `zm attach` brings you back, `zm ls` lists sessions with the
  ones that need you first. Each session records its agent, command, model and worktree, and ends with a typed
  outcome (`ok`, `quota`, `auth`, `failed` and a few more), so a usage-limit stop is never reported as success.
- **Claude Code and Codex, on every machine.** `zm install claude --yes` and `zm install codex --yes` add hooks
  that register each session and tell it when mail is waiting (without `--yes` they only show the change; Codex runs
  the hooks once you trust them in its `/hooks`), and `zm mcp` gives any MCP client the same verbs as the command. Make
  one always-on machine a team project's home and invite teammates' machines with a one-time code: claims, mail and
  the list of sessions then reach across all of them over your Tailscale tailnet.

## Works with Beads

If your agents already use [Beads](https://github.com/steveyegge/beads)' `bd` commands, Mezze's `bd-zm` answers
them from the project's native issues (`zm tracker add native --prefix bd` turns those on), with Beads' flags and
output for the daily commands. Link it as `bd` and your agents keep typing what they know; on that machine the link
hides a real Beads `bd`, so do it only if your issues live in zm. It does not run, bundle or need Beads, and it does not import issues from a Beads database.

## Who can answer a question

Only a paired phone makes an answer provably a person's. On one machine, every program running as your user can
reach the daemon's socket, so an agent there can answer an ask too; the receipt then records that the machine's
socket took the answer, not that a person pressed a button. A pull request's ask, and a choice that runs something
when picked, are refused from the socket and need a signed answer, such as the paired phone's. Pair one before you let a receipt gate a
merge.

## Source and install

Mezze's licence is Apache-2.0. Its source is not public yet. When it is, the quick start will look like this
(from Mezze's README; you need Go 1.27.4 or newer, plus git and make for the full install):

```sh
git clone https://github.com/sengirunners/mezze && cd mezze && make install

cd ~/code/myapp
zm init                                    # starts the daemon and makes this repo a project
zm work new "Add the down migration"       # -> created myapp/local:k3x9qa
zm run --work local:k3x9qa claude          # the agent runs holding the item; detach with ctrl-]
zm send local:k3x9qa "the migration needs a down step"   # reaches whoever holds the item
zm ls                                      # sessions, the ones that need you first
```

`make install` puts `zm`, `bd-zm`, the drivers that lend a machine to a Potluck pool and `potluck-conformance` in
Go's bin directory (`$(go env GOBIN)`, or `$(go env GOPATH)/bin`), which you add to your `PATH`. `zm doctor`
checks the machine's setup and says how to fix what is missing.

## Talk to us

To use Mezze before its source is public, tell us what you run: use the form on [the home page](/#founders) and pick
"Try Mezze early", or write to ops@sengi.run. Jackson and Clay, who make Potluck and Mezze, read every message.

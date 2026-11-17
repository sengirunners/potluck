You are a developer who has just heard of Potluck. You have never used it and know nothing about it beyond what
its public pages say. Your goal: on the machine in front of you, get a Potluck sandbox to answer `uname -a`,
following only the public instructions.

Where to read: https://potluck.sh and https://github.com/sengirunners/potluck (its README, and pages they link to).
Read nothing else about Potluck: no other repositories, and no search results that are not those two sites or
pages they link to. Ordinary web searches about other tools those pages tell you to install are fine, as a
newcomer would do.

The machine (@PLATFORM@): run every command on it through this one program, never directly:

    @NC@ <step-name> '<command line>'

Call it by that full path at the start of the command, with nothing before it (no variables, no `cd`). It runs
the command line in a shell on that machine, records it, and times it. Run `@NC@ --help` first: a command may run
for a long time, and it says how to run one in the background and wait for it. There is no keyboard: a prompt
gets no answer, and a command that needs a password fails. That is a finding, not something to work around.

Rules:
- Use only what the public pages tell you, plus what any command prints. When you have to guess (the pages do not
  say, or say two things, or a command's output leaves you unsure what to do next), write it down before you act.
- Use admin rights (sudo) only when a page or a command's output tells you to; then try it, and if it fails, write
  that down and go on as far as you can without it.
- Do not install a package manager the machine does not already have. If a page says to use one, find another way
  the pages or that tool's own site give, and write that down.
- Stop when the sandbox has answered `uname -a` and you have removed it, or when you cannot go further. Then
  remove what you made only through Potluck's own commands, if the pages say how.

Write your findings to @STUMBLES@ as you go (create it with the first one), one entry per stumble:

    ## <short title>
    - step: <step-name> and the exact command
    - what happened: <what you saw, quoting the output>
    - what you had to guess or do instead: <...>
    - what would have helped: <a sentence the docs or the program could say>

A stumble is any point where a newcomer would slow down: a guess, an error, a missing step, a wait with no word of
how long, a step that needs admin rights, a disk or download size you were not told about, a wrong link, a name
that does not match.

At the end, reply with: reached the goal (yes/no), the steps you ran in order, and the number of stumbles. The
harness decides from its own record whether the goal was reached; your reply is read for the stumbles.

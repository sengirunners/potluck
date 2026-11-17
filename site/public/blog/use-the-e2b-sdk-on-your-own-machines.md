# Use the E2B SDK on your own machines

Published 2026-11-17. https://potluck.sh/blog/use-the-e2b-sdk-on-your-own-machines

If you have code written against E2B's SDK and spare machines on your team, you can run that code's sandboxes on
those machines without changing it. `potluck-e2b` is a small gateway that the Potluck installer puts beside
`potluck`. It answers the E2B control plane and the parts of a sandbox's own API the SDK uses, and turns them into
Potluck sandboxes.

## Set it up

You need a Potluck pool with a machine sharing itself (the [quick start](https://github.com/sengirunners/potluck#quick-start),
or the prompt on the [agent sandbox page](/agent-sandbox)). Then, on a machine in the pool:

```sh
potluck-e2b keygen     # makes an API key and prints it; keys live in $POTLUCK_HOME/e2b/keys (mode 0600)
potluck-e2b            # serves on localhost:49982 and prints the lines to export
```

On start it prints three things to set in the program that uses the SDK: `E2B_DOMAIN` (by default
`e2b.localhost:49982`), `E2B_API_KEY` (a key from the keys file), and `SSL_CERT_FILE` for Python on Linux or
`NODE_EXTRA_CA_CERTS` for Node, naming the certificate authority the gateway made for itself (on macOS the Python SDK
trusts only the keychain). Code that uses only the calls listed below needs no other change; network rules, pause,
volumes, PTY, stdin streaming and lower-case environment variable names are refused. Names under `.localhost` always point at your own machine, and the gateway signs a certificate for each
name it serves, limited to its own domain.

## What it covers

- `Sandbox.create`, placed on your pool's machines the way `potluck sandbox create` places them;
- `commands.run`, with its exit code, standard output and standard error;
- `files.read` and `files.write`;
- `get_info`, `Sandbox.list`, `set_timeout`, `Sandbox.connect` and `kill`.

A sandbox's environment variables reach a command as secret files, never on its command line, so the secrets rules
apply: they go only to a machine you trust whose sandboxes can hold secrets.

## How it is checked

A test runs the unmodified E2B Python and TypeScript SDKs against the gateway: it creates a sandbox, runs commands,
writes and reads a file, lists, connects and kills (`TestRealSDK` in internal/e2bgateway).

## When to use E2B instead

E2B runs the machines for you, in its cloud, and covers more of its own API. Use it when you have no spare machines
or need more sandboxes at once than your team owns. Use `potluck-e2b`
when the machines are already yours and you would rather your code ran on them.

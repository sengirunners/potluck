# Potluck in the Official MCP Registry

Potluck's MCP server (`potluck mcp`, [README](../README.md#mcp-for-agents)) is listed in the
[Official MCP Registry](https://registry.modelcontextprotocol.io) as `io.github.sengirunners/potluck`. Directories
such as PulseMCP and MCP clients read their listings from it.

The registry lists only packages, so the entry names an MCP bundle (`registryType: mcpb`), `potluck.mcpb`:

| file | what |
| --- | --- |
| `bundle/manifest.json` | the bundle's manifest ([MCPB](https://github.com/modelcontextprotocol/mcpb)); its `version` is the bundle's |
| `bundle/server/potluck-mcp` | the launcher: runs the potluck on the user's machine (`$POTLUCK_BIN` or the bundle's "potluck program" setting, else `PATH`, else `~/.local/bin`) as `potluck [--pool $POTLUCK_MCP_POOL] mcp`, or says how to install it |
| `server.json` | the registry entry; `fileSha256` is filled in by `build.sh` |
| `build.sh <out>` | builds `<out>/potluck.mcpb` (the same bytes for the same files) and `<out>/server.json` |

The bundle carries no potluck binary, so it does not change with Potluck releases and the server always matches the
daemon it talks to. It lives on its own release, `mcp-bundle-v<version>`, a prerelease that is never marked latest
and is not pruned (release.yml prunes `potluck-main-*` only), so `install.sh` and `potluck upgrade` never see it.

## Publishing

Once, before the first run: the public repository's tag rulesets must allow `refs/tags/mcp-bundle-v*`
(`docs/public-sync.md`, "Settings the public repository keeps"); without it the release step is refused.

1. Change what you need, and raise `version` in both `bundle/manifest.json` and `server.json` (and the version in
   `server.json`'s `identifier`); `build.sh` refuses when they disagree.
2. Merge, and publish the commit to the public repository (`docs/public-sync.md`).
3. In `sengirunners/potluck`, run the `mcp-registry` workflow from main (Actions, or
   `gh workflow run mcp-registry.yml -R sengirunners/potluck`). It releases the bundle, then publishes the entry,
   signing in with the job's GitHub OIDC token. A version already released or published is left alone.

Check: `curl 'https://registry.modelcontextprotocol.io/v0.1/servers/io.github.sengirunners%2Fpotluck/versions/latest'`.

Validate locally: `mcp-registry/build.sh /tmp/mcpb && mcp-publisher validate /tmp/mcpb/server.json`, and
`npx @anthropic-ai/mcpb validate mcp-registry/bundle/manifest.json`.

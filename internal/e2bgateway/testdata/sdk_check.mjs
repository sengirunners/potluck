// The W2 check with the unmodified e2b TypeScript SDK: only the environment
// points it at zm-e2b (E2B_DOMAIN, E2B_API_KEY, NODE_EXTRA_CA_CERTS).
import { Sandbox, CommandExitError } from 'e2b'

const sbx = await Sandbox.create({ timeoutMs: 120_000 })
const r = await sbx.commands.run('uname -a')
console.log('uname exit', r.exitCode, 'out', r.stdout.trim() !== '')
await sbx.files.write('hello.txt', 'hello from zm\n')
console.log('read', (await sbx.files.read('hello.txt')).trim())
const info = await sbx.getInfo()
console.log('info', info.sandboxId === sbx.sandboxId, info.state)
const listed = await Sandbox.list().nextItems()
console.log('listed', listed.some((s) => s.sandboxId === sbx.sandboxId))
await sbx.setTimeout(300_000)
console.log('connect', (await Sandbox.connect(sbx.sandboxId)).sandboxId === sbx.sandboxId)
try {
  await sbx.commands.run("head -c 1100000 /dev/zero | tr '\\0' a")
} catch (e) {
  console.log('cap', String(e).includes('1048576 byte cap'))
}
try {
  await sbx.commands.run('echo oops >&2; exit 3')
} catch (e) {
  if (!(e instanceof CommandExitError)) throw e
  console.log('exit', e.exitCode, e.stderr.trim())
}
await sbx.kill()
const after = await Sandbox.list().nextItems()
console.log('killed', !after.some((s) => s.sandboxId === sbx.sandboxId))

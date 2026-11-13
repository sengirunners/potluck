# The W2 check with the unmodified e2b Python SDK: only the environment
# points it at zm-e2b (E2B_DOMAIN, E2B_API_KEY, SSL_CERT_FILE).
from e2b import Sandbox, CommandExitException

sbx = Sandbox.create(timeout=120)
r = sbx.commands.run("uname -a")
print("uname exit", r.exit_code, "out", bool(r.stdout.strip()))
sbx.files.write("hello.txt", "hello from zm\n")
print("read", sbx.files.read("hello.txt").strip())
info = sbx.get_info()
print("info", info.sandbox_id == sbx.sandbox_id, info.state)
print("listed", any(s.sandbox_id == sbx.sandbox_id for s in Sandbox.list().next_items()))
sbx.set_timeout(300)
print("connect", Sandbox.connect(sbx.sandbox_id).sandbox_id == sbx.sandbox_id)
try:
    sbx.commands.run("head -c 1100000 /dev/zero | tr '\\0' a")
except Exception as e:
    print("cap", "1048576 byte cap" in str(e))
try:
    sbx.commands.run("echo oops >&2; exit 3")
except CommandExitException as e:
    print("exit", e.exit_code, e.stderr.strip())
sbx.kill()
print("killed", not any(s.sandbox_id == sbx.sandbox_id for s in Sandbox.list().next_items()))

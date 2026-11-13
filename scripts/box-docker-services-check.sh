#!/bin/sh
# Run INSIDE an expendable docker=on Kata box. Targets are host:port pairs
# confirmed reachable from the producer: host, neighbor, LAN, tailnet, metadata.
# It never configures the producer or changes another box.
set -eu
: "${POTLUCK_DOCKER_FORBID:?set five reachable host:port controls (host neighbor LAN tailnet metadata)}"
set -- $POTLUCK_DOCKER_FORBID
[ "$#" -ge 5 ] || { echo 'need host, neighbor, LAN, tailnet and metadata controls' >&2; exit 1; }
prefix=potluck-services-$$
work=$(mktemp -d)
cleanup() {
  docker rm -f "$prefix-web" "$prefix-db" >/dev/null 2>&1 || true
  docker network rm "$prefix" >/dev/null 2>&1 || true
  docker image rm "$prefix" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
for attempt in $(seq 1 90); do
  docker info >/dev/null 2>&1 && break
  sleep 1
done
docker info >/dev/null
# The test image builds with normal bridge networking (apk reaches its registry).
cat > "$work/Dockerfile" <<'DOCKER'
FROM python:3.13-alpine
RUN apk add --no-cache postgresql-client
COPY web.py /web.py
CMD ["python", "/web.py"]
DOCKER
cat > "$work/web.py" <<'PY'
import http.server, subprocess
class Web(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        result = subprocess.check_output(['psql', '-h', 'db', '-U', 'postgres', '-Atc', 'select 42'])
        self.send_response(200)
        self.end_headers()
        self.wfile.write(result)
http.server.HTTPServer(('0.0.0.0', 8080), Web).serve_forever()
PY
docker build -t "$prefix" "$work"
docker network create "$prefix"
docker run -d --name "$prefix-db" --network "$prefix" --network-alias db \
  -e POSTGRES_PASSWORD=potluck-check postgres:17-alpine
for attempt in $(seq 1 60); do
  docker exec "$prefix-db" pg_isready -h 127.0.0.1 -U postgres && break
  sleep 1
done
docker exec "$prefix-db" pg_isready -h 127.0.0.1 -U postgres
docker run -d --name "$prefix-web" --network "$prefix" --network-alias web \
  -e PGPASSWORD=potluck-check -p 127.0.0.1::8080 "$prefix"
# A third service resolves web, whose request resolves db and queries Postgres.
for attempt in $(seq 1 30); do
  if docker run --rm --network "$prefix" "$prefix" python -c \
    'import urllib.request; assert urllib.request.urlopen("http://web:8080").read().strip() == b"42"'; then
    break
  fi
  sleep 1
done
docker run --rm --network "$prefix" "$prefix" python -c \
  'import urllib.request; assert urllib.request.urlopen("http://web:8080").read().strip() == b"42"'
port=$(docker port "$prefix-web" 8080/tcp | sed 's/.*://')
printf 'GET / HTTP/1.0\r\n\r\n' | nc -w 10 127.0.0.1 "$port" | grep -q '^42'
# Root in the OUTER guest must still lack packet/address/device capabilities.
sudo -n perl -e 'open my $f, "<", "/proc/self/status" or die $!; while (<$f>) { if (/^CapBnd:\s+(\S+)/) { die "unsafe CapBnd $1" if hex($1) & ((1<<12)|(1<<13)|(1<<27)); exit 0; } } die "missing CapBnd";' 
for target in "$@"; do
  host=${target%:*}; port=${target##*:}
  if sudo -n nc -z -w 3 "$host" "$port"; then
    echo "outer root reached forbidden $target" >&2; exit 1
  fi
  # Even a nested container holding every capability gets only its own
  # namespace, not the host. (--privileged also wants a writable /sys, which
  # a Kata box's read-only /sys refuses: nested privileged mode is unavailable.)
  docker run --rm --cap-add ALL --network "$prefix" "$prefix" python -c '
import socket, sys
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=3)
except OSError:
    pass
else:
    raise SystemExit("nested root reached forbidden " + sys.argv[1])
' "$host" "$port"
done
echo 'PASS: ordinary build, service DNS, web to Postgres, published guest port, outer and nested root fence'

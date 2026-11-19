#!/usr/bin/env python3
"""Cooperative task proxy. The caller owns the process group's lifetime.

No firewall is installed. A client can ignore its proxy environment settings.
"""
import ipaddress
import json
import os
import re
import resource
import signal
import socket
import socketserver
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit


class Refused(Exception):
    pass


def hostname(value):
    value = value.lower().rstrip('.')
    if len(value) > 253 or not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+', value):
        raise Refused('invalid DNS name')
    if re.fullmatch(r'(?:[0-9]+|0x[0-9a-f]+)', value.rsplit('.', 1)[-1]):
        raise Refused('IP addresses are not allowed')
    return value


def allowed(host, entries):
    return any(host == entry or (entry.startswith('*.') and host.endswith(entry[1:])) for entry in entries)


def public_address(text):
    address = ipaddress.ip_address(text)
    if isinstance(address, ipaddress.IPv6Address):
        # Translation must not turn a private IPv4 destination into an
        # apparently public IPv6 address. Teredo and local-use NAT64 prefixes
        # do not provide an unambiguous public destination here.
        if address.teredo or address in ipaddress.ip_network('64:ff9b:1::/48'):
            return False
        if address.ipv4_mapped:
            return public_address(str(address.ipv4_mapped))
        if address in ipaddress.ip_network('64:ff9b::/96'):
            return public_address(str(ipaddress.IPv4Address(int(address) & 0xffffffff)))
        if address.sixtofour and not public_address(str(address.sixtofour)):
            return False
        if address.is_site_local:
            return False
    return (address.is_global and not address.is_multicast and not address.is_reserved
            and not address.is_loopback and not address.is_link_local and not address.is_unspecified)


def connect_public(host, port):
    addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    if not addresses or any(not public_address(a[4][0]) for a in addresses):
        raise Refused('destination does not resolve exclusively to public addresses')
    for family, kind, proto, _, address in addresses:
        upstream = socket.socket(family, kind, proto)
        upstream.settimeout(10)
        try:
            upstream.connect(address)  # Use the checked address, without resolving again.
            return upstream
        except OSError:
            upstream.close()
    raise Refused('destination connection failed')


def read_exact(sock, size):
    result = bytearray()
    while len(result) < size:
        part = sock.recv(size - len(result))
        if not part:
            raise Refused('incomplete TLS hello')
        result.extend(part)
    return bytes(result)


def tls_hello(sock, host):
    """Require a single-record ClientHello naming the CONNECT destination.

    Fragmented hellos and encrypted client hellos are refused, never guessed.
    """
    head = read_exact(sock, 5)
    size = int.from_bytes(head[3:5], 'big')
    if head[0] != 22 or head[1] != 3 or size > 16384:
        raise Refused('expected a TLS hello')
    body = read_exact(sock, size)
    try:
        if body[0] != 1 or int.from_bytes(body[1:4], 'big') != len(body)-4:
            raise Refused('expected one complete ClientHello')
        pos = 38
        pos += 1 + body[pos]  # session id
        count = int.from_bytes(body[pos:pos+2], 'big')
        pos += 2 + count  # cipher suites
        pos += 1 + body[pos]  # compression methods
        end = pos + 2 + int.from_bytes(body[pos:pos+2], 'big')
        pos += 2
        if end != len(body):
            raise Refused('invalid TLS extensions')
        names = []
        seen = set()
        while pos < end:
            kind = int.from_bytes(body[pos:pos+2], 'big')
            length = int.from_bytes(body[pos+2:pos+4], 'big')
            pos += 4
            data = body[pos:pos+length]
            pos += length
            if kind in seen or pos > end or kind == 0xfe0d:
                raise Refused('duplicate, truncated or encrypted TLS extension')
            seen.add(kind)
            if kind == 0:
                if len(data) < 5 or int.from_bytes(data[:2], 'big') != len(data)-2:
                    raise Refused('invalid TLS server name list')
                at = 2
                while at < len(data):
                    tag = data[at]
                    length = int.from_bytes(data[at+1:at+3], 'big')
                    at += 3
                    name = data[at:at+length]
                    at += length
                    if tag != 0 or at > len(data):
                        raise Refused('invalid TLS server name')
                    names.append(hostname(name.decode('ascii')))
        if names != [host]:
            raise Refused('TLS server name differs from CONNECT destination')
    except (IndexError, UnicodeError):
        raise Refused('invalid TLS hello') from None
    return head + body


class DownloadRate:
    def __init__(self, mbit):
        self.rate = mbit * 1000000 / 8
        self.lock = threading.Lock()
        self.next = time.monotonic()

    def wait(self, size):
        if not self.rate:
            return
        with self.lock:
            now = time.monotonic()
            self.next = max(now, self.next) + size / self.rate
            delay = self.next - now
        time.sleep(delay)


class Proxy(socketserver.ThreadingTCPServer):
    daemon_threads = True
    block_on_close = False

    def __init__(self, config):
        self.hosts = config['hosts']
        self.rate = DownloadRate(config['download_mbit'])
        self.connections = threading.BoundedSemaphore(64)
        super().__init__(('127.0.0.1', 0), Request)

    def process_request(self, request, address):
        if not self.connections.acquire(blocking=False):
            request.close()
            return
        super().process_request(request, address)

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            self.connections.release()


class Request(socketserver.BaseRequestHandler):
    def handle(self):
        client = self.request
        client.settimeout(10)
        upstream = None
        connected = False
        try:
            # Read only headers: bytes after CONNECT belong to the TLS hello.
            header = bytearray()
            while not header.endswith(b'\r\n\r\n'):
                if len(header) >= 32768:
                    raise Refused('request headers too large')
                piece = client.recv(1)
                if not piece:
                    return
                header.extend(piece)
            lines = header.decode('ascii').split('\r\n')
            method, target, version = lines[0].split(' ')
            if version not in ('HTTP/1.0', 'HTTP/1.1'):
                raise Refused('unsupported HTTP version')
            fields = []
            for line in lines[1:-2]:
                key, value = line.split(':', 1)
                if not re.fullmatch(r'[A-Za-z0-9-]+', key) or any(ord(c) < 32 and c != '\t' for c in value):
                    raise Refused('invalid request header')
                fields.append((key.lower(), value.strip()))
            if any(key in ('transfer-encoding', 'upgrade') or (key == 'content-length' and value != '0') for key, value in fields):
                raise Refused('request bodies and upgrades are unsupported')
            if method == 'CONNECT':
                name, port = target.rsplit(':', 1)
                if port != '443':
                    raise Refused('CONNECT requires port 443')
                host = hostname(name)
            else:
                url = urlsplit(target)
                if method not in ('GET', 'HEAD') or url.scheme != 'http' or url.username or url.password or url.fragment or url.port not in (None, 80):
                    raise Refused('plain HTTP supports GET and HEAD on port 80 only')
                host, port = hostname(url.hostname or ''), '80'
            if not allowed(host, self.server.hosts):
                raise Refused('destination is not on the cooperative proxy allowlist')
            if method == 'CONNECT':
                client.sendall(b'HTTP/1.1 200 Connection Established\r\n\r\n')
                connected = True
                hello = tls_hello(client, host)
                upstream = connect_public(host, 443)
                upstream.sendall(hello)
            else:
                upstream = connect_public(host, 80)
                path = url.path or '/'
                if url.query:
                    path += '?' + url.query
                clean = [(key, value) for key, value in fields if key not in
                         ('host', 'connection', 'proxy-connection', 'proxy-authorization', 'content-length')]
                request = f'{method} {path} HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n'
                request += ''.join(f'{key}: {value}\r\n' for key, value in clean) + '\r\n'
                upstream.sendall(request.encode('ascii'))
            client.settimeout(None)
            upstream.settimeout(None)
            if method == 'CONNECT':
                threading.Thread(target=self.pipe, args=(client, upstream, False), daemon=True).start()
            self.pipe(upstream, client, True)
        except (Refused, ValueError, OSError) as error:
            # Never log request headers, URLs, credentials or upstream bytes.
            if not connected:
                try:
                    client.sendall(b'HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
                except OSError:
                    pass
            print('potluck: cooperative proxy refused a request (' + type(error).__name__ + ')', file=sys.stderr)
        finally:
            if upstream:
                upstream.close()

    def pipe(self, source, destination, download):
        try:
            while True:
                data = source.recv(16384)
                if not data:
                    break
                if download:
                    self.server.rate.wait(len(data))
                destination.sendall(data)
        except OSError:
            pass
        finally:
            try:
                destination.shutdown(socket.SHUT_WR)
            except OSError:
                pass


def main():
    # The launcher opens credentials only in the child after this process starts.
    # Close inherited descriptors defensively, without opening any secret file.
    maximum = resource.getrlimit(resource.RLIMIT_NOFILE)[0]
    os.closerange(3, maximum if maximum != resource.RLIM_INFINITY else 1048576)
    config_path = sys.argv[1]
    with open(config_path, encoding='utf8') as config_file:
        config = json.load(config_file)
    server = Proxy(config)  # A bind/startup failure prevents the agent starting.
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    env = os.environ.copy()
    address = 'http://127.0.0.1:' + str(server.server_address[1])
    for name in ('HTTP_PROXY', 'http_proxy', 'HTTPS_PROXY', 'https_proxy', 'ALL_PROXY', 'all_proxy'):
        env[name] = address
    env['NO_PROXY'] = env['no_proxy'] = 'localhost,127.0.0.1,::1'
    print('potluck: cooperative proxy ready; clients can bypass it; downloads ' +
          str(config['download_mbit']) + ' Mbit/s (0 is unlimited)', file=sys.stderr)
    child = subprocess.Popen(sys.argv[2:], env=env, close_fds=True)
    def stopped(signum, frame):
        # timeout signals the whole group; this also handles a direct signal.
        if child.poll() is None:
            child.send_signal(signum)
    signal.signal(signal.SIGTERM, stopped)
    signal.signal(signal.SIGINT, stopped)
    try:
        code = child.wait()
    finally:
        server.shutdown()
        server.server_close()
    return code if code >= 0 else 128-code


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError):
        print('potluck: cooperative proxy could not start', file=sys.stderr)
        sys.exit(125)

"""Loopback-only behavioral tests; no public DNS or network requests."""
import importlib.util
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

SCRIPT = sys.argv.pop(1)
spec = importlib.util.spec_from_file_location('taskproxy', SCRIPT)
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)


class Behavior(unittest.TestCase):
    def setUp(self):
        self.server = proxy.Proxy({'hosts': ['example.com', '*.actions.githubusercontent.com', '*.cdn.example.com'], 'download_mbit': 0})
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def request(self, data):
        conn = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        conn.settimeout(2)
        conn.connect(self.server.server_address)
        conn.sendall(data)
        self.addCleanup(conn.close)
        return conn

    def origin(self, body):
        downstream, upstream = socket.socketpair()
        def respond():
            try:
                request = bytearray()
                while not request.endswith(b'\r\n\r\n'):
                    request.extend(upstream.recv(1))
                self.assertIn(b'Host: example.com\r\n', request)
                self.assertNotIn(b'Proxy-Authorization', request)
                upstream.sendall(body)
            finally:
                upstream.close()
        threading.Thread(target=respond, daemon=True).start()
        return downstream

    def test_http_allowed_and_paced(self):
        self.server.rate = proxy.DownloadRate(1)
        body = b'HTTP/1.1 200 OK\r\nContent-Length: 62500\r\n\r\n' + b'x'*62500
        with patch.object(proxy, 'connect_public', side_effect=lambda host, port:self.origin(body)) as connect:
            start = time.monotonic()
            clients = [self.request(b'GET http://example.com/a HTTP/1.1\r\nHost: evil.com\r\nProxy-Authorization: secret\r\n\r\n') for _ in range(2)]
            for conn in clients:
                got = bytearray()
                while True:
                    data = conn.recv(65536)
                    if not data:
                        break
                    got.extend(data)
                self.assertEqual(bytes(got), body)
            # Both downloads share the same 1 Mbit/s budget, not one each.
            self.assertGreaterEqual(time.monotonic()-start, .9)
            self.assertEqual(connect.call_count, 2)
            connect.assert_called_with('example.com', 80)

    def test_denials_never_connect(self):
        with patch.object(proxy, 'connect_public', side_effect=AssertionError('unexpected network')):
            for request in [
                b'GET http://evil.com/ HTTP/1.1\r\n\r\n',
                b'CONNECT 127.0.0.1:443 HTTP/1.1\r\n\r\n',
                b'CONNECT example.com:22 HTTP/1.1\r\n\r\n',
                b'POST http://example.com/ HTTP/1.1\r\nContent-Length: 1\r\n\r\nx',
                b'GET http://example.com/ HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n',
                b'GET http://actions.githubusercontent.com/ HTTP/1.1\r\n\r\n',
                b'GET http://cdn.example.com/ HTTP/1.1\r\n\r\n',
                b'GET http://notcdn.example.com/ HTTP/1.1\r\n\r\n',
            ]:
                self.assertTrue(self.request(request).recv(4096).startswith(b'HTTP/1.1 403'))

    def hello(self, hostname):
        incoming, outgoing = ssl.MemoryBIO(), ssl.MemoryBIO()
        client = ssl.create_default_context().wrap_bio(incoming, outgoing, server_side=False, server_hostname=hostname)
        try:
            client.do_handshake()
        except ssl.SSLWantReadError:
            pass
        return outgoing.read()

    def test_connect_sni(self):
        for hostname, should_connect in [('evil.com', False), ('example.com', True)]:
            peers = []
            def connect(host, port):
                a,b = socket.socketpair()
                peers.append(b)
                b.sendall(b'upstream')
                b.shutdown(socket.SHUT_WR)
                return a
            with patch.object(proxy, 'connect_public', side_effect=connect) as dial:
                conn = self.request(b'CONNECT example.com:443 HTTP/1.1\r\n\r\n')
                self.assertIn(b'200 Connection Established', conn.recv(4096))
                conn.sendall(self.hello(hostname))
                response = conn.recv(4096)
                self.assertEqual(response, b'upstream' if should_connect else b'')
                self.assertEqual(dial.called, should_connect)
            for peer in peers:
                peer.close()

    def test_private_dns_refused(self):
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, 6, '', ('127.0.0.1',80))]
        with patch.object(proxy.socket, 'getaddrinfo', return_value=addresses):
            conn = self.request(b'GET http://example.com/ HTTP/1.1\r\n\r\n')
            self.assertTrue(conn.recv(4096).startswith(b'HTTP/1.1 403'))

    def test_translated_and_multicast_dns_never_connect(self):
        for address in ['224.0.0.1', 'ff02::1', 'ff0e::1', '64:ff9b::a9fe:a9fe',
                        '64:ff9b::7f00:1', '64:ff9b:1::808:808', '::ffff:169.254.169.254',
                        '2002:a9fe:a9fe::', '2001:0000:4136:e378:8000:63bf:3fff:fdd2',
                        'fec0::1', '100.64.0.1']:
            with self.subTest(address=address):
                family = socket.AF_INET6 if ':' in address else socket.AF_INET
                destination = (address,80,0,0) if family == socket.AF_INET6 else (address,80)
                # Even a mixed answer with a public address first must be
                # rejected before creating any outbound socket.
                answers = [(socket.AF_INET,socket.SOCK_STREAM,6,'',('8.8.8.8',80)),
                           (family,socket.SOCK_STREAM,6,'',destination)]
                with patch.object(proxy.socket,'getaddrinfo',return_value=answers), \
                     patch.object(proxy.socket,'socket',side_effect=AssertionError('outbound dial forbidden')) as dial:
                    with self.assertRaises(proxy.Refused):
                        proxy.connect_public('example.com',80)
                    dial.assert_not_called()

    def test_scoped_wildcard_connect(self):
        downstream, upstream = socket.socketpair()
        self.addCleanup(upstream.close)
        upstream.sendall(b'HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n')
        upstream.shutdown(socket.SHUT_WR)
        with patch.object(proxy,'connect_public',return_value=downstream) as dial:
            conn = self.request(b'GET http://asset.cdn.example.com/ HTTP/1.1\r\n\r\n')
            self.assertTrue(conn.recv(4096).startswith(b'HTTP/1.1 200'))
            dial.assert_called_once_with('asset.cdn.example.com',80)

    def test_launcher_and_startup_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory)/'config.json'
            config.write_text(json.dumps({'hosts':['example.com'], 'download_mbit':48}))
            child = "import os,urllib.request; p=os.environ['HTTPS_PROXY']; assert p.startswith('http://127.0.0.1:'); assert os.environ['http_proxy']==p; print('child-ran'); raise SystemExit(7)"
            result = subprocess.run([sys.executable, SCRIPT, str(config), sys.executable, '-c', child], capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 7, result.stderr)
            self.assertIn(b'child-ran', result.stdout)
            config.write_text('invalid')
            result = subprocess.run([sys.executable, SCRIPT, str(config), sys.executable, '-c', "print('never')"], capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 125)
            self.assertNotIn(b'never', result.stdout)


unittest.main()

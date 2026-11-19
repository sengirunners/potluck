import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('bootstrap', Path(__file__).with_name('database_start.py'))
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class SnapshotBootstrap(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / 'home'
        self.home.mkdir()
        self.artifact = self.root / 'artifact'
        self.artifact.mkdir()
        self.config = self.root / 'config'
        self.config.mkdir()
        (self.config / 'postgresql.conf').write_text("data_directory='/var/lib/postgresql/data'\n")
        data = self.root / 'snapshot'
        data.mkdir()
        (data / 'PG_VERSION').write_text('17\n')
        for name in ('base', 'global', 'pg_wal', 'pg_xact'):
            (data / name).mkdir()
        (data / 'global/pg_control').write_bytes(b'fake control')
        with tarfile.open(self.artifact / 'pgdata.tar.gz', 'w:gz') as archive:
            archive.add(data, arcname='.')
        self.m = json.loads((Path(__file__).parent.parent / 'database/testdata/manifest.json').read_text())
        self.m['arch'] = {'x86_64': 'amd64', 'arm64': 'arm64', 'aarch64': 'arm64'}[platform.machine()]
        self.m['snapshot_sha256'] = bootstrap.sha(self.artifact / 'pgdata.tar.gz')
        bindir = self.root / 'bin'
        bindir.mkdir()
        self.m['runtime']['bindir'] = str(bindir)
        # Executed fake programs model the PostgreSQL command interfaces. They
        # never open sockets or run a server, and expose each rejected oracle.
        script = '''#!/usr/bin/env python3
import os, pathlib, sys
name=pathlib.Path(sys.argv[0]).name
if name == 'pg_controldata': print('Database cluster state: shut down')
elif name == 'pg_ctl': pass
elif name == 'psql':
 if '-c' in sys.argv:
  assert os.environ.get('PGPASSWORD'); print('1')
 else:
  q=sys.stdin.read()
  if q.startswith('show server_version'): print('17.6')
  elif q.startswith('select version'): print('00001')
  elif q.startswith('select coalesce'): print('{"pgtap":"1.3.0"}')
  elif not q.startswith('alter role postgres password '): sys.exit(2)
'''
        for name in ('pg_controldata', 'pg_ctl', 'psql'):
            path = bindir / name
            path.write_text(script)
            path.chmod(0o700)

    def start(self):
        manifest = self.artifact / 'manifest.json'
        manifest.write_text(json.dumps(self.m))
        with patch.object(bootstrap.os, 'getuid', return_value=1001), patch.object(bootstrap.platform, 'machine', return_value={'amd64':'x86_64','arm64':'aarch64'}[self.m['arch']]):
            bootstrap.start(self.artifact, self.config, self.home, bootstrap.sha(manifest))

    def test_private_copy_and_application_connection(self):
        original = bootstrap.sha(self.artifact / 'pgdata.tar.gz')
        self.start()
        private = self.home / '.potluck-database'
        url = (private / 'url').read_text()
        self.assertRegex(url, r'^postgresql://postgres:[a-f0-9]{64}@127.0.0.1:5432/postgres\n$')
        self.assertEqual((private / 'url').stat().st_mode & 0o777, 0o600)
        self.assertEqual(private.stat().st_mode & 0o777, 0o700)
        self.assertEqual(bootstrap.sha(self.artifact / 'pgdata.tar.gz'), original)
        self.assertIn(str(private / 'data'), (private / 'config/postgresql.conf').read_text())
        with self.assertRaises(FileExistsError):
            self.start()

    def test_declared_configuration_sources_without_conf_extension(self):
        (self.config / 'postgresql.conf').write_text("include='/etc/postgresql/settings.inc'\n")
        (self.config / 'settings.inc').write_text("include='/var/lib/postgresql/data/limits.conf'\n")
        data = self.root / 'snapshot'
        (data / 'limits.conf').write_text('max_connections=20\n')
        (data / 'postgresql.auto.conf').write_text("data_directory='/var/lib/postgresql/data'\n")
        with tarfile.open(self.artifact / 'pgdata.tar.gz', 'w:gz') as archive:
            archive.add(data, arcname='.')
        self.m['snapshot_sha256'] = bootstrap.sha(self.artifact / 'pgdata.tar.gz')
        self.m['config_files']['sources'] += ['/etc/postgresql/settings.inc', '/var/lib/postgresql/data/limits.conf']
        self.start()
        private = self.home / '.potluck-database'
        self.assertIn(str(private / 'config/settings.inc'), (private / 'config/postgresql.conf').read_text())
        self.assertIn(str(private / 'data/limits.conf'), (private / 'config/settings.inc').read_text())
        self.assertIn(str(private / 'data'), (private / 'data/postgresql.auto.conf').read_text())
        self.assertTrue((private / 'url').exists())

    def test_snapshot_checksum_refused_before_thaw(self):
        self.m['snapshot_sha256'] = '0' * 64
        with self.assertRaisesRegex(RuntimeError, 'snapshot checksum mismatch'):
            self.start()
        self.assertFalse((self.home / '.potluck-database').exists())

    def test_wrong_migration_ledger_never_exposes_url(self):
        self.m['inputs'] = {'supabase/migrations/00002_wrong.sql': 'a' * 64}
        with self.assertRaisesRegex(RuntimeError, 'migration ledger mismatch'):
            self.start()
        self.assertFalse((self.home / '.potluck-database/url').exists())

    def test_wrong_extensions_never_exposes_url(self):
        self.m['extensions']['pgtap'] = '99'
        with self.assertRaisesRegex(RuntimeError, 'extension versions mismatch'):
            self.start()
        self.assertFalse((self.home / '.potluck-database/url').exists())

    def test_snapshot_symlink_refused(self):
        with tarfile.open(self.artifact / 'pgdata.tar.gz', 'w:gz') as archive:
            member = tarfile.TarInfo('pg_wal')
            member.type = tarfile.SYMTYPE
            member.linkname = '/tmp/shared'
            archive.addfile(member)
        self.m['snapshot_sha256'] = bootstrap.sha(self.artifact / 'pgdata.tar.gz')
        with self.assertRaisesRegex(RuntimeError, 'unsafe database snapshot entry'):
            self.start()
        self.assertFalse((self.home / '.potluck-database/url').exists())


if __name__ == '__main__':
    unittest.main()

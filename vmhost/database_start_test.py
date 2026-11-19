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
        self.system = self.root / 'system'
        self.config = self.system / 'etc/postgresql'
        self.config.mkdir(parents=True)
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
  user=sys.argv[sys.argv.index('-U') + 1]
  if q.startswith('show server_version'): print('17.6')
  elif q.startswith('select version'): print(os.environ.get('FAKE_LEDGER', '00001'))
  elif q.startswith('select coalesce'): print('{"pgtap":"1.3.0"}')
  elif q.startswith('select rolname'): print('supabase_admin')
  # As in Supabase, only the bootstrap superuser may set postgres's password.
  elif not (q.startswith('alter role postgres password ') and user == 'supabase_admin'): sys.exit(2)
'''
        for name in ('pg_controldata', 'pg_ctl', 'psql'):
            path = bindir / name
            path.write_text(script)
            path.chmod(0o700)

    def start(self):
        manifest = self.artifact / 'manifest.json'
        manifest.write_text(json.dumps(self.m))
        with patch.object(bootstrap.os, 'getuid', return_value=1001), patch.object(bootstrap.platform, 'machine', return_value={'amd64':'x86_64','arm64':'aarch64'}[self.m['arch']]):
            bootstrap.start(self.artifact, self.system, self.home, bootstrap.sha(manifest))

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

    def test_sibling_configuration_root(self):
        # Supabase's postgresql.conf includes files from /etc/postgresql-custom and names
        # directories there; a root that is not declared is left as it is.
        custom = self.system / 'etc/postgresql-custom'
        (custom / 'conf.d').mkdir(parents=True)
        (custom / 'extension-custom-scripts').mkdir()
        (self.config / 'postgresql.conf').write_text(
            "include = '/etc/postgresql-custom/supautils.conf'\n"
            "include_dir = '/etc/postgresql-custom/conf.d'\n"
            "hba_file = '/etc/postgresql/pg_hba.conf'\n"
            "other = '/etc/postgresql-other/x'\n"
            "nested = '/opt/etc/postgresql/x:/mnt/var/lib/postgresql/data/y'\n"
            "data_directory = '/var/lib/postgresql/data'\n")
        (custom / 'supautils.conf').write_text("supautils.extension_custom_scripts_path = '/etc/postgresql-custom/extension-custom-scripts'\n")
        (custom / 'conf.d/limits.conf').write_text('max_connections=20\n')
        self.m['config_archives']['/etc/postgresql-custom'] = {'archive': 'postgres-custom-config.tar.gz', 'sha256': 'd' * 64}
        self.m['config_files']['sources'] += ['/etc/postgresql-custom/supautils.conf', '/etc/postgresql-custom/conf.d/limits.conf']
        self.start()
        private = self.home / '.potluck-database'
        main = (private / 'config/postgresql.conf').read_text()
        self.assertIn(f"include = '{private}/config-custom/supautils.conf'", main)
        self.assertIn(f"include_dir = '{private}/config-custom/conf.d'", main)
        self.assertIn(f"hba_file = '{private}/config/pg_hba.conf'", main)
        self.assertIn("other = '/etc/postgresql-other/x'", main)
        self.assertIn("nested = '/opt/etc/postgresql/x:/mnt/var/lib/postgresql/data/y'", main)
        self.assertIn(f"data_directory = '{private}/data'", main)
        self.assertEqual((private / 'config-custom/supautils.conf').read_text(),
                         f"supautils.extension_custom_scripts_path = '{private}/config-custom/extension-custom-scripts'\n")
        self.assertTrue((private / 'config-custom/extension-custom-scripts').is_dir())
        self.assertTrue((private / 'url').exists())

    def test_undeclared_configuration_root_refused(self):
        self.m['config_files']['sources'] += ['/etc/postgresql-custom/supautils.conf']
        with self.assertRaisesRegex(RuntimeError, 'outside packaged roots'):
            self.start()
        self.assertFalse((self.home / '.potluck-database/url').exists())

    def test_snapshot_checksum_refused_before_thaw(self):
        self.m['snapshot_sha256'] = '0' * 64
        with self.assertRaisesRegex(RuntimeError, 'snapshot checksum mismatch'):
            self.start()
        self.assertFalse((self.home / '.potluck-database').exists())

    def test_wrong_migration_ledger_never_exposes_url(self):
        self.m['migration_versions_sha256'] = hashlib.sha256(b'00002\n').hexdigest()
        with self.assertRaisesRegex(RuntimeError, 'migration ledger mismatch'):
            self.start()
        self.assertFalse((self.home / '.potluck-database/url').exists())

    def test_ledger_digest_is_order_free_and_counts(self):
        # The server may answer in any order; the digest is over byte order.
        self.m['migration_count'] = 3
        self.m['migration_versions_sha256'] = hashlib.sha256(b'00001\n00002\n00010\n').hexdigest()
        with patch.dict(os.environ, {'FAKE_LEDGER': '00010\n00001\n00002'}):
            self.start()
        self.assertTrue((self.home / '.potluck-database/url').exists())

    def test_ledger_count_mismatch_refused(self):
        self.m['migration_count'] = 2
        with self.assertRaisesRegex(RuntimeError, 'migration ledger mismatch'):
            self.start()

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

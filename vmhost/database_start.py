# Run by the driver's supervisor before the box becomes ready.
import hashlib
import json
import os
import pathlib
import platform
import re
import secrets
import shutil
import shlex
import subprocess
import sys
import tarfile


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(path):
    with open(path, 'rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def run(args, data=None):
    result = subprocess.run(args, input=data, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=120)
    require(result.returncode == 0, 'database command failed: ' + pathlib.Path(args[0]).name)
    return result.stdout.strip()


# A configuration root the image may carry; database/manifest.go's ConfigRootRE is the same rule.
CONFIG_ROOT = re.compile(r'/etc/postgresql(-[a-z0-9]+)?')


def start(artifact, system, home, manifest_digest):
    # system is the guest's /: the manifest's configuration roots are read beneath it.
    require(os.getuid() != 0, 'database needs an unprivileged guest user')
    manifest = artifact / 'manifest.json'
    require(sha(manifest) == manifest_digest, 'database manifest checksum mismatch')
    m = json.loads(manifest.read_text())
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    require(m['arch'] == arch, 'database architecture mismatch')
    require(sha(artifact / 'pgdata.tar.gz') == m['snapshot_sha256'], 'database snapshot checksum mismatch')
    root = home / '.potluck-database'
    root.mkdir(mode=0o700)  # Existing state is never reused.
    data = root / 'data'
    data.mkdir(mode=0o700)
    with tarfile.open(artifact / 'pgdata.tar.gz', 'r:gz') as archive:
        for entry in archive.getmembers():
            path = pathlib.PurePosixPath(entry.name)
            require(not path.is_absolute() and '..' not in path.parts and
                    (entry.isdir() or entry.isfile()), 'unsafe database snapshot entry')
        archive.extractall(data, filter='data')
    require((data / 'PG_VERSION').read_text().strip() == '17', 'unsupported PostgreSQL version')
    require(not (data / 'postmaster.pid').exists(), 'snapshot is still running')
    for name in ('base', 'global', 'pg_wal', 'pg_xact'):
        require((data / name).is_dir(), 'incomplete full database snapshot')
    bindir = pathlib.Path(m['runtime']['bindir'])
    control = run([str(bindir / 'pg_controldata'), str(data)])
    require(re.search(r'^Database cluster state:\s+shut down\s*$', control, re.M), 'snapshot is not cleanly stopped')
    old_data = m['pgdata_path']
    require(old_data.startswith('/') and '..' not in old_data.split('/'), 'invalid original PGDATA')
    # Each configuration root is copied whole: /etc/postgresql to config, /etc/postgresql-custom to config-custom.
    roots = {}
    for original in sorted(m.get('config_archives') or {}):
        require(CONFIG_ROOT.fullmatch(original), 'database configuration outside packaged roots')
        roots[original] = root / ('config' + original[len('/etc/postgresql'):])
        shutil.copytree(system / original.lstrip('/'), roots[original], symlinks=False)
    # PostgreSQL includes need not end in .conf. Relocate the producer's
    # complete source inventory, and the cluster's automatic settings file.
    def relocated(original):
        for old, new in [(old_data, data), *roots.items()]:
            if original.startswith(old + '/'):
                return new / original[len(old) + 1:]
        raise RuntimeError('database configuration outside packaged roots')
    # One pass, longest name first, and only whole path components: /etc/postgresql
    # never rewrites inside /etc/postgresql-custom, and a rewritten path is never rewritten again.
    moves = {old_data: str(data), **{old: str(new) for old, new in roots.items()}}
    pattern = re.compile(r'(?<![A-Za-z0-9_./-])(' + '|'.join(re.escape(old) for old in sorted(moves, key=len, reverse=True)) + r')(?![A-Za-z0-9_.-])')
    sources = set(m['config_files']['sources'] or [])
    sources.add(m['config_files']['config'])
    if (data / 'postgresql.auto.conf').exists():
        sources.add(old_data + '/postgresql.auto.conf')
    for original in sources:
        path = relocated(original)
        require(path.is_file(), 'missing database configuration source')
        path.write_text(pattern.sub(lambda match: moves[match.group(1)], path.read_text()))
    config_file = relocated(m['config_files']['config'])
    hba = root / 'pg_hba.conf'
    hba.write_text('local all all trust\nhost all all 127.0.0.1/32 scram-sha-256\n')
    socket = root / 'socket'
    socket.mkdir(mode=0o700)
    # Explicit options win over the copied server settings. No published port,
    # logical subscription worker, cron job or external server is started.
    options = [item for key, value in {
        'config_file': config_file, 'data_directory': data,
        'hba_file': hba, 'unix_socket_directories': socket,
        'listen_addresses': '127.0.0.1', 'port': '5432',
        'ssl': 'off', 'archive_mode': 'off', 'max_logical_replication_workers': '0',
        'cron.launch_active_jobs': 'off', 'password_encryption': 'scram-sha-256',
        'external_pid_file': root / 'postgres.pid',
    }.items() for item in ('-c', f'{key}={value}')]
    run([str(bindir / 'pg_ctl'), '-D', str(data), '-l', str(root / 'postgres.log'),
         '-w', '-t', '90', '-o', shlex.join(options), 'start'])
    psql = [str(bindir / 'psql'), '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', str(socket), '-U', 'postgres', '-d', 'postgres']
    def sql(query):
        return run(psql, query)
    require(sql('show server_version;') == m['postgres_version'], 'database server version mismatch')
    # The producer records a digest of its ledger, not one entry per migration file:
    # the versions in byte order, each followed by a newline.
    versions = sorted(sql('select version from supabase_migrations.schema_migrations;').splitlines())
    ledger = hashlib.sha256(''.join(v + '\n' for v in versions).encode()).hexdigest()
    require(versions and len(versions) == len(set(versions)) == m['migration_count'] and
            ledger == m['migration_versions_sha256'], 'database migration ledger mismatch')
    extensions = json.loads(sql("select coalesce(jsonb_object_agg(extname, extversion), '{}'::jsonb) from pg_extension;"))
    require(extensions == m['extensions'], 'database extension versions mismatch')
    password = secrets.token_hex(32)
    # Supabase's postgres role is not a superuser and cannot set its own password;
    # the cluster's bootstrap superuser (oid 10) can.
    admin = sql('select rolname from pg_roles where oid = 10;')
    require(re.fullmatch(r'[a-z_][a-z0-9_]{0,62}', admin), 'database superuser not found')
    run([str(bindir / 'psql'), '-XAt', '-v', 'ON_ERROR_STOP=1', '-h', str(socket), '-U', admin, '-d', 'postgres'],
        "alter role postgres password '" + password + "';")
    url = 'postgresql://postgres:' + password + '@127.0.0.1:5432/postgres'
    # Prove the application's connection path before making its credential visible.
    env = dict(os.environ, PGPASSWORD=password)
    check = subprocess.run([str(bindir / 'psql'), '-XAt', '-h', '127.0.0.1', '-U', 'postgres', '-d', 'postgres', '-c', 'select 1'],
                           env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
    require(check.returncode == 0 and check.stdout.strip() == b'1', 'database application connection failed')
    with open(root / 'url', 'x', opener=lambda path, flags: os.open(path, flags, 0o600)) as f:
        f.write(url + '\n')


if __name__ == '__main__':
    try:
        start(pathlib.Path('/opt/potluck-db'), pathlib.Path('/'), pathlib.Path.home(), sys.argv[1])
    except Exception as error:
        # Never print subprocess output: a database can echo credentials.
        print('database snapshot: ' + str(error), file=sys.stderr)
        sys.exit(1)

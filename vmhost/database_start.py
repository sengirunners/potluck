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


def start(artifact, configuration, home, manifest_digest):
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
    config = root / 'config'
    shutil.copytree(configuration, config, symlinks=False)
    # PostgreSQL includes need not end in .conf. Relocate the producer's
    # complete source inventory, and the cluster's automatic settings file.
    def relocated(original):
        if original.startswith(old_data + '/'):
            return data / original[len(old_data) + 1:]
        if original.startswith('/etc/postgresql/'):
            return config / original[len('/etc/postgresql/'):]
        raise RuntimeError('database configuration outside packaged roots')
    sources = set(m['config_files']['sources'] or [])
    sources.add(m['config_files']['config'])
    if (data / 'postgresql.auto.conf').exists():
        sources.add(old_data + '/postgresql.auto.conf')
    for original in sources:
        path = relocated(original)
        require(path.is_file(), 'missing database configuration source')
        text = path.read_text().replace(old_data, str(data)).replace('/etc/postgresql', str(config))
        path.write_text(text)
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
    versions = sql('select version from supabase_migrations.schema_migrations order by version;').splitlines()
    expected = sorted(path.split('/')[-1].split('_')[0] for path in m['inputs']
                      if re.fullmatch(r'supabase/migrations/[^/]+\.sql', path))
    require(expected and len(expected) == len(set(expected)) and versions == expected and
            len(versions) == m['migration_count'], 'database migration ledger mismatch')
    extensions = json.loads(sql("select coalesce(jsonb_object_agg(extname, extversion), '{}'::jsonb) from pg_extension;"))
    require(extensions == m['extensions'], 'database extension versions mismatch')
    password = secrets.token_hex(32)
    sql("alter role postgres password '" + password + "';")
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
        start(pathlib.Path('/opt/potluck-db'), pathlib.Path('/etc/postgresql'), pathlib.Path.home(), sys.argv[1])
    except Exception as error:
        # Never print subprocess output: a database can echo credentials.
        print('database snapshot: ' + str(error), file=sys.stderr)
        sys.exit(1)

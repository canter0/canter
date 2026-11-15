import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import signal
import shutil
import statistics
import subprocess
import tarfile
import time


def utc_now():
    return datetime.now(timezone.utc).isoformat()


class Benchmark:
    def __init__(self, repository, commit, output, profile, repeats, timeout):
        self.repository = repository.resolve()
        self.commit = commit
        self.output = output.resolve()
        self.profile = profile
        self.repeats = repeats
        self.deadline = time.monotonic() + timeout
        self.environment = {
            key: value for key, value in os.environ.items()
            if key in {'PATH', 'HOME', 'LANG', 'LC_ALL', 'TMPDIR', 'SSL_CERT_FILE', 'SSL_CERT_DIR'}
        }
        self.report = {
            'schemaVersion': 1,
            'status': 'running',
            'startedAt': utc_now(),
            'commit': commit,
            'profile': profile,
            'repeats': repeats,
            'scope': 'Source export, dependency downloads, and application builds; not the full CI test suite.',
            'coldDefinition': 'Fresh application/build caches per pair; dependencies already downloaded. OS page caches are not cleared.',
            'warmDefinition': 'Immediate repeat in the same source directory with the same build caches.',
            'host': {'platform': platform.platform(), 'architecture': platform.machine(), 'logicalCPUs': os.cpu_count()},
            'steps': [],
        }

    def save(self):
        (self.output / 'results.json').write_text(json.dumps(self.report, indent=2) + '\n')

    def remaining(self):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError('Benchmark time limit reached')
        return remaining

    def command(self, name, args, cwd, environment=None):
        step = {'name': name, 'command': args, 'startedAt': utc_now()}
        self.report['steps'].append(step)
        log_path = self.output / f'{len(self.report["steps"]):02d}-{name}.log'
        step['log'] = log_path.name
        start = time.perf_counter()
        process = None
        try:
            with log_path.open('wb') as log:
                process = subprocess.Popen(args, cwd=cwd, env=environment or self.environment,
                                           stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                step['exitCode'] = process.wait(timeout=self.remaining())
        except (subprocess.TimeoutExpired, KeyboardInterrupt, OSError, RuntimeError):
            if process is not None:
                self.stop(process)
            step['status'] = 'failed'
            raise RuntimeError(f'{name} did not finish; inspect its log') from None
        finally:
            step['seconds'] = time.perf_counter() - start
            self.save()
        if step['exitCode'] != 0:
            step['status'] = 'failed'
            self.save()
            raise RuntimeError(f'{name} failed; inspect its log')
        step['status'] = 'success'
        self.save()
        return log_path.read_text(errors='replace').strip()

    @staticmethod
    def stop(process):
        try:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        except ProcessLookupError:
            pass

    def snapshot(self, name):
        destination = self.output / name
        destination.mkdir()
        with tarfile.open(self.output / 'source.tar') as archive:
            for member in archive:
                relative = PurePosixPath(member.name)
                if relative.is_absolute() or '..' in relative.parts or not (member.isdir() or member.isfile()):
                    raise RuntimeError('Source archive contains an unsupported path or entry type')
                target = destination.joinpath(*relative.parts)
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    with archive.extractfile(member) as source, target.open('xb') as output:
                        shutil.copyfileobj(source, output)
                    target.chmod(member.mode & 0o777)
        return destination

    def run(self):
        self.output.mkdir(parents=True, exist_ok=False)
        self.save()
        try:
            actual = self.command('verify-commit', ['git', 'rev-parse', '--verify', self.commit + '^{commit}'],
                                  self.repository)
            if actual != self.commit:
                raise RuntimeError('Source commit did not match the requested full SHA')
            self.command('source-export', ['git', 'archive', '--format=tar', '--output=' + str(self.output / 'source.tar'),
                                          self.commit], self.repository)
            source = self.snapshot('dependencies')
            for key, path in [('cgroupCPU', '/sys/fs/cgroup/cpu.max'),
                              ('cgroupMemory', '/sys/fs/cgroup/memory.max'),
                              ('memory', '/proc/meminfo')]:
                if Path(path).is_file():
                    self.report['host'][key] = Path(path).read_text()
            if Path('/proc/cpuinfo').is_file():
                models = {line.split(':', 1)[1].strip() for line in Path('/proc/cpuinfo').read_text().splitlines()
                          if line.startswith('model name') and ':' in line}
                self.report['host']['cpuModels'] = sorted(models)
            go_environment = dict(self.environment, GOMODCACHE=str(self.output / 'go-modules'),
                                  GOPATH=str(self.output / 'go-path'), GOTOOLCHAIN='auto')
            if self.profile in {'go', 'all'}:
                go_environment['GOCACHE'] = str(self.output / 'go-setup-cache')
                self.report['goVersion'] = self.command('go-toolchain-setup', ['go', 'env', 'GOVERSION'], source,
                                                        go_environment)
                go_mod = (source / 'go.mod').read_text()
                expected = re.search(r'^go ([0-9.]+)$', go_mod, re.MULTILINE)
                if not expected or self.report['goVersion'] != 'go' + expected.group(1):
                    raise RuntimeError('Go toolchain must match the go.mod version exactly')
                self.command('go-dependencies', ['go', 'mod', 'download'], source, go_environment)
            if self.profile in {'web', 'all'}:
                self.report['nodeVersion'] = self.command('node-version', ['node', '--version'], source)
                self.report['pnpmVersion'] = self.command('pnpm-version', ['pnpm', '--version'], source)
                package = json.loads((source / 'web/package.json').read_text())
                if not self.report['nodeVersion'].startswith('v22.'):
                    raise RuntimeError('Use Node 22 to match existing CI')
                if package.get('packageManager') != 'pnpm@' + self.report['pnpmVersion']:
                    raise RuntimeError('pnpm version must match package.json exactly')
                self.command('web-dependencies', ['pnpm', 'fetch', '--frozen-lockfile',
                                                 '--store-dir', str(self.output / 'pnpm-store')], source / 'web')
            for index in range(1, self.repeats + 1):
                pair = self.snapshot(f'pair-{index}')
                if self.profile in {'go', 'all'}:
                    environment = dict(go_environment, GOCACHE=str(self.output / f'go-cache-{index}'),
                                       CGO_ENABLED='0')
                    args = ['go', 'build', '-mod=readonly', '-trimpath', '-o',
                            str(self.output / f'controlplane-{index}'), './cmd/canter-controlplane']
                    self.command(f'go-cold-{index}', args, pair, environment)
                    self.command(f'go-warm-{index}', args, pair, environment)
                if self.profile in {'web', 'all'}:
                    self.command(f'web-install-{index}', ['pnpm', 'install', '--offline', '--frozen-lockfile',
                                                         '--store-dir', str(self.output / 'pnpm-store')], pair / 'web')
                    environment = dict(self.environment, NEXT_TELEMETRY_DISABLED='1')
                    self.command(f'web-cold-{index}', ['pnpm', 'build'], pair / 'web', environment)
                    self.command(f'web-warm-{index}', ['pnpm', 'build'], pair / 'web', environment)
            summaries = {}
            for workload in ['go-cold', 'go-warm', 'web-install', 'web-cold', 'web-warm']:
                values = [step['seconds'] for step in self.report['steps']
                          if step['name'].startswith(workload + '-') and step.get('status') == 'success']
                if values:
                    summaries[workload] = {'samples': len(values), 'medianSeconds': statistics.median(values),
                                           'minSeconds': min(values), 'maxSeconds': max(values)}
            self.report.update(status='success', summaries=summaries)
        except (RuntimeError, OSError, ValueError, tarfile.TarError, KeyboardInterrupt) as exc:
            self.report.update(status='failed', error=str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__)
            raise
        finally:
            self.report['finishedAt'] = utc_now()
            self.save()


def main():
    parser = argparse.ArgumentParser(description='Measure pinned-source Go/Next builds with isolated cold/warm caches.')
    parser.add_argument('--repository-dir', type=Path, required=True)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--profile', choices=['go', 'web', 'all'], default='all')
    parser.add_argument('--repeats', type=int, choices=range(1, 6), default=3)
    parser.add_argument('--timeout-seconds', type=int, default=3600)
    args = parser.parse_args()
    if not re.fullmatch(r'[0-9a-f]{40}', args.commit):
        parser.error('--commit must be a full lowercase 40-character commit SHA')
    if not 60 <= args.timeout_seconds <= 5400:
        parser.error('--timeout-seconds must be 60..5400')
    if args.output.exists():
        parser.error('--output must not already exist')
    def interrupt(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupt)
    try:
        benchmark = Benchmark(args.repository_dir, args.commit, args.output, args.profile, args.repeats,
                              args.timeout_seconds)
        benchmark.run()
    except (RuntimeError, OSError, ValueError, tarfile.TarError, KeyboardInterrupt):
        print('Benchmark failed; no successful speed claim. Inspect the output results.json and step logs.')
        return 1
    print(json.dumps(benchmark.report['summaries'], indent=2))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())

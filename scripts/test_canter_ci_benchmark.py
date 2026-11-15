import importlib.util
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('canter_ci_benchmark', Path(__file__).with_name('canter-ci-benchmark.py'))
benchmark = importlib.util.module_from_spec(spec)
spec.loader.exec_module(benchmark)

SHA = 'a' * 40


class BenchmarkTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.run = benchmark.Benchmark(self.root, SHA, self.root / 'results', 'all', 3, 60)
        self.calls = []

    def fake_command(self, name, args, cwd, environment=None):
        self.calls.append((name, args, cwd, environment))
        if name == 'source-export':
            with tarfile.open(self.run.output / 'source.tar', 'w') as archive:
                for name, content in [('go.mod', 'module example.test/ci\n\ngo 1.26.8\n'),
                                      ('web/package.json', '{"packageManager":"pnpm@11.19.0"}')]:
                    data = content.encode()
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    archive.addfile(info, io.BytesIO(data))
        step_name = self.calls[-1][0]
        self.run.report['steps'].append({'name': step_name, 'seconds': float(len(self.calls)), 'status': 'success'})
        return {'verify-commit': SHA, 'go-toolchain-setup': 'go1.26.8',
                'node-version': 'v22.21.0', 'pnpm-version': '11.19.0'}.get(step_name, '')

    def test_paired_caches_pinned_source_and_medians(self):
        with patch.object(self.run, 'command', side_effect=self.fake_command):
            self.run.run()
        self.assertEqual(self.calls[0][1], ['git', 'rev-parse', '--verify', SHA + '^{commit}'])
        self.assertEqual(self.calls[1][1][-1], SHA)
        environments = []
        for index in range(1, 4):
            cold = next(call for call in self.calls if call[0] == f'go-cold-{index}')
            warm = next(call for call in self.calls if call[0] == f'go-warm-{index}')
            self.assertEqual(cold[1:], warm[1:])
            self.assertEqual(cold[3]['CGO_ENABLED'], '0')
            environments.append(cold[3]['GOCACHE'])
            install = next(call for call in self.calls if call[0] == f'web-install-{index}')
            self.assertIn('--offline', install[1])
            self.assertIn('--frozen-lockfile', install[1])
        self.assertEqual(len(set(environments)), 3)
        report = json.loads((self.run.output / 'results.json').read_text())
        self.assertEqual(report['status'], 'success')
        self.assertEqual(report['summaries']['go-cold'], {
            'samples': 3, 'medianSeconds': 13.0, 'minSeconds': 8.0, 'maxSeconds': 18.0})
        self.assertNotIn('.git', [path.name for path in (self.run.output / 'pair-1').iterdir()])

    def test_mismatched_source_never_builds(self):
        with patch.object(self.run, 'command', return_value='b' * 40):
            with self.assertRaisesRegex(RuntimeError, 'commit'):
                self.run.run()
        self.assertEqual(self.run.report['status'], 'failed')
        self.assertNotIn('summaries', self.run.report)

    def test_wrong_tool_versions_are_not_comparable_runs(self):
        for stage in ['go-toolchain-setup', 'node-version', 'pnpm-version']:
            with self.subTest(stage=stage):
                self.run.output = self.root / stage
                def command(name, args, cwd, environment=None):
                    result = self.fake_command(name, args, cwd, environment)
                    return 'wrong-version' if name == stage else result
                with patch.object(self.run, 'command', side_effect=command):
                    with self.assertRaises(RuntimeError):
                        self.run.run()
                self.assertEqual(self.run.report['status'], 'failed')
                self.assertNotIn('summaries', self.run.report)

    def test_command_records_real_elapsed_time_and_failure(self):
        self.run.output.mkdir()
        start = time.perf_counter()
        text = self.run.command('success', [sys.executable, '-c', 'print("ok")'], self.root)
        elapsed = time.perf_counter() - start
        self.assertEqual(text, 'ok')
        self.assertEqual(self.run.report['steps'][0]['exitCode'], 0)
        self.assertGreater(self.run.report['steps'][0]['seconds'], 0)
        self.assertLessEqual(self.run.report['steps'][0]['seconds'], elapsed)
        with self.assertRaisesRegex(RuntimeError, 'failure failed'):
            self.run.command('failure', [sys.executable, '-c', 'raise SystemExit(7)'], self.root)
        self.assertEqual(self.run.report['steps'][-1]['exitCode'], 7)
        self.assertEqual(self.run.report['steps'][-1]['status'], 'failed')

    def test_timeout_is_a_failure_not_a_fast_result(self):
        self.run.output.mkdir()
        self.run.deadline = time.monotonic() + 0.1
        with self.assertRaisesRegex(RuntimeError, 'did not finish'):
            self.run.command('timeout', [sys.executable, '-c', 'import time; time.sleep(30)'], self.root)
        self.assertEqual(self.run.report['steps'][-1]['status'], 'failed')
        self.assertNotIn('exitCode', self.run.report['steps'][-1])

    def test_existing_output_is_never_overwritten(self):
        self.run.output.mkdir()
        sentinel = self.run.output / 'results.json'
        sentinel.write_text('preserve')
        with self.assertRaises(FileExistsError):
            self.run.run()
        self.assertEqual(sentinel.read_text(), 'preserve')

    def test_archive_traversal_and_links_are_rejected(self):
        self.run.output.mkdir()
        for index, (name, kind) in enumerate([('../escaped', tarfile.REGTYPE),
                                             ('/absolute', tarfile.REGTYPE),
                                             ('symlink', tarfile.SYMTYPE),
                                             ('hardlink', tarfile.LNKTYPE)]):
            with self.subTest(name=name):
                with tarfile.open(self.run.output / 'source.tar', 'w') as archive:
                    info = tarfile.TarInfo(name)
                    info.type = kind
                    info.linkname = '/tmp/target'
                    archive.addfile(info)
                with self.assertRaisesRegex(RuntimeError, 'unsupported'):
                    self.run.snapshot(f'unsafe-{index}')
                self.assertFalse((self.run.output / 'escaped').exists())


if __name__ == '__main__':
    unittest.main()

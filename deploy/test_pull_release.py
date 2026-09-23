import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release', Path(__file__).with_name('pull-release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)

class ActivationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.root = root / 'opt'
        self.state = root / 'state'
        self.state.mkdir()
        (self.root / 'bin').mkdir(parents=True)
        (self.root / 'bin/canter-controlplane').write_text('old-binary')
        (self.root / 'web').mkdir()
        (self.root / 'web/page').write_text('old-web')
        self.next = root / 'release'
        (self.next / 'web').mkdir(parents=True)
        (self.next / 'web/page').write_text('new-web')
        (self.next / 'canter-controlplane').write_text('new-binary')
        self.sha = 'a' * 40
        self.harness = root / 'harness'
        self.systemd = root / 'systemd'
        self.systemd.mkdir()
        (self.next / 'harness/node_modules/just-bash').mkdir(parents=True)
        (self.next / 'harness/runner.mjs').write_text('new-runner')
        (self.next / 'harness/node_modules/just-bash/package.json').write_text('{}')
        (self.next / 'deploy').mkdir()
        for name in release.HARNESS_UNITS:
            (self.next / 'deploy' / name).write_text('new-unit')
        (self.next / 'canter-static-linux').write_text('new-static')
        p = patch.object(release, 'unit_state', return_value=False)
        p.start()
        self.addCleanup(p.stop)
        for name, value in [('ROOT', self.root), ('STATE', self.state), ('HARNESS', self.harness), ('SYSTEMD', self.systemd)]:
            p = patch.object(release, name, value)
            p.start()
            self.addCleanup(p.stop)

    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=True)
    def test_success_retains_backup_and_records_commit(self, health, run, subprocess):
        release.activate(self.next, self.sha)
        self.assertEqual((self.root / 'bin/canter-controlplane').read_text(), 'new-binary')
        self.assertEqual((self.root / 'web/page').read_text(), 'new-web')
        self.assertEqual((self.next / 'previous-web/page').read_text(), 'old-web')
        self.assertEqual((self.state / 'current').read_text(), self.sha)
        self.assertEqual((self.root / 'bin/canter-static-linux').read_text(), 'new-static')
        self.assertEqual((self.harness / 'current/runner.mjs').read_text(), 'new-runner')
        self.assertTrue((self.harness / 'current').is_symlink())
        run.assert_any_call('systemctl', 'enable', '--now', 'canter-harness.socket')
        self.assertNotIn(('systemctl', 'stop', 'canter-harness.socket'), [c.args for c in run.call_args_list])
        self.assertTrue(any('pg_restore' in call.args[0] for call in subprocess.call_args_list))

    @patch.object(release.time, 'sleep')
    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=False)
    def test_failed_health_restores_previous_release(self, health, run, subprocess, sleep):
        with self.assertRaisesRegex(RuntimeError, 'readiness'):
            release.activate(self.next, self.sha)
        self.assertEqual((self.root / 'bin/canter-controlplane').read_text(), 'old-binary')
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')
        self.assertEqual((self.state / 'failed').read_text(), self.sha)
        self.assertFalse((self.state / 'current').exists())
        self.assertFalse((self.root / 'bin/canter-static-linux').exists())
        self.assertFalse((self.harness / 'current').is_symlink())
        for name in release.HARNESS_UNITS:
            self.assertFalse((self.systemd / name).exists())

    @patch.object(release.subprocess, 'run', side_effect=RuntimeError('backup failed'))
    @patch.object(release, 'run')
    def test_backup_failure_does_not_stop_production(self, run, subprocess):
        with self.assertRaisesRegex(RuntimeError, 'backup failed'):
            release.activate(self.next, self.sha)
        run.assert_not_called()
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')

    @patch.object(release.time, 'sleep')
    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    @patch.object(release, 'healthy', return_value=False)
    def test_failed_upgrade_restores_existing_runtime(self, health, run, subprocess, sleep):
        old = self.harness / 'releases/old'
        old.mkdir(parents=True)
        (old / 'runner.mjs').write_text('old-runner')
        (self.harness / 'current').symlink_to(old)
        (self.root / 'bin/canter-static-linux').write_text('old-static')
        for name in release.HARNESS_UNITS:
            (self.systemd / name).write_text('old-' + name)
        with patch.object(release, 'unit_state', return_value=True):
            with self.assertRaisesRegex(RuntimeError, 'readiness'):
                release.activate(self.next, self.sha)
        self.assertEqual((self.harness / 'current').resolve(), old.resolve())
        self.assertEqual((self.root / 'bin/canter-static-linux').read_text(), 'old-static')
        for name in release.HARNESS_UNITS:
            self.assertEqual((self.systemd / name).read_text(), 'old-' + name)
        run.assert_any_call('systemctl', 'start', 'canter-harness.socket')

    @patch.object(release.subprocess, 'run')
    @patch.object(release, 'run')
    def test_missing_runtime_artifact_restores_application(self, run, subprocess):
        (self.next / 'canter-static-linux').unlink()
        with self.assertRaisesRegex(RuntimeError, 'required runtime artifact'):
            release.activate(self.next, self.sha)
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')
        self.assertEqual((self.root / 'bin/canter-controlplane').read_text(), 'old-binary')
        run.assert_any_call('systemctl', 'start', 'canter-controlplane', 'canter-web')

if __name__ == '__main__':
    unittest.main()

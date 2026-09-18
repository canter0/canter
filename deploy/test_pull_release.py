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
        for name, value in [('ROOT', self.root), ('STATE', self.state)]:
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

    @patch.object(release.subprocess, 'run', side_effect=RuntimeError('backup failed'))
    @patch.object(release, 'run')
    def test_backup_failure_does_not_stop_production(self, run, subprocess):
        with self.assertRaisesRegex(RuntimeError, 'backup failed'):
            release.activate(self.next, self.sha)
        run.assert_not_called()
        self.assertEqual((self.root / 'web/page').read_text(), 'old-web')

if __name__ == '__main__':
    unittest.main()

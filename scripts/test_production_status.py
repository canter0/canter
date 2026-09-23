import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('production_status', Path(__file__).with_name('production-status.py'))
status = importlib.util.module_from_spec(spec)
spec.loader.exec_module(status)


class PrivateAccessTests(unittest.TestCase):
    def setUp(self):
        self.tailnet = {'BackendState': 'Running', 'Peer': {'node': {
            'HostName': 'canter-production', 'DNSName': 'canter-production.example.ts.net.',
            'Online': True, 'TailscaleIPs': ['100.68.35.38']}}}

    def test_verified_private_alias(self):
        self.assertTrue(status.verify_private_target(self.tailnet, 'hostname 100.68.35.38\nuser root')['Online'])

    def test_public_fallback_and_proxy_are_rejected(self):
        for config in ['hostname 203.0.113.1', 'hostname 100.68.35.38\nproxycommand nc other 22',
                       'hostname 100.68.35.38\nproxyjump public-bastion']:
            with self.subTest(config=config), self.assertRaisesRegex(RuntimeError, 'verified Tailscale'):
                status.verify_private_target(self.tailnet, config)

    def test_offline_and_signed_out_require_reconnection(self):
        self.tailnet['Peer']['node']['Online'] = False
        with self.assertRaisesRegex(RuntimeError, 'unavailable'):
            status.verify_private_target(self.tailnet, 'hostname 100.68.35.38')
        self.tailnet['BackendState'] = 'NeedsLogin'
        with self.assertRaisesRegex(RuntimeError, 'not connected'):
            status.verify_private_target(self.tailnet, 'hostname 100.68.35.38')


if __name__ == '__main__':
    unittest.main()

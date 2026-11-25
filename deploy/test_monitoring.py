import json
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).parent
MONITORING = ROOT / 'monitoring'


class MonitoringConfigurationTests(unittest.TestCase):
    def test_install_script_is_posix_shell_and_pins_the_grafana_key(self):
        script = (ROOT / 'install-monitoring.sh').read_text()
        subprocess.run(['sh', '-n', str(ROOT / 'install-monitoring.sh')], check=True)
        self.assertIn('B53AE77BADB630A683046005963FA27710458545', script)
        self.assertIn('https://apt.grafana.com/gpg-full.key', script)
        self.assertIn('promtool check config', script)
        self.assertIn('grafana_version=', script)
        self.assertIn('Dpkg::Options::=--force-confold', script)
        self.assertIn('systemctl restart prometheus prometheus-node-exporter grafana-server', script)
        self.assertIn('wait_for_url grafana http://127.0.0.1:3001/api/health 120', script)
        self.assertNotIn('tailscale funnel', script)

    def test_prometheus_scrapes_only_loopback_targets_with_bounded_retention(self):
        config = (MONITORING / 'prometheus.yml').read_text()
        defaults = (MONITORING / 'prometheus.default').read_text()
        for target in ('127.0.0.1:9090', '127.0.0.1:9100', '127.0.0.1:8081'):
            self.assertIn(target, config)
        for job in ('job_name: prometheus', 'job_name: node', 'job_name: canter-controlplane'):
            self.assertIn(job, config)
        self.assertIn('metrics_path: /metrics', config)
        self.assertNotIn('0.0.0.0', config)
        self.assertIn('--web.listen-address=127.0.0.1:9090', defaults)
        self.assertIn('--storage.tsdb.retention.time=7d', defaults)
        self.assertIn('--storage.tsdb.retention.size=1GB', defaults)

    def test_node_exporter_and_grafana_are_loopback_only(self):
        node = (MONITORING / 'prometheus-node-exporter.default').read_text()
        grafana = (MONITORING / 'grafana-server.service.conf').read_text()
        datasource = (MONITORING / 'grafana-datasource.yaml').read_text()
        provider = (MONITORING / 'grafana-dashboard-provider.yaml').read_text()
        self.assertIn('--web.listen-address=127.0.0.1:9100', node)
        self.assertNotIn('0.0.0.0', node)
        self.assertIn('GF_SERVER_HTTP_ADDR=127.0.0.1', grafana)
        self.assertIn('GF_SERVER_HTTP_PORT=3001', grafana)
        self.assertIn('GF_USERS_ALLOW_SIGN_UP=false', grafana)
        self.assertIn('GF_AUTH_ANONYMOUS_ENABLED=false', grafana)
        self.assertIn('GF_ANALYTICS_REPORTING_ENABLED=false', grafana)
        self.assertIn('GF_ANALYTICS_CHECK_FOR_UPDATES=false', grafana)
        self.assertIn('GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false', grafana)
        self.assertIn('EnvironmentFile=/etc/canter-monitoring/grafana.env', grafana)
        self.assertNotIn('PASSWORD=', grafana)
        self.assertIn('url: http://127.0.0.1:9090', datasource)
        self.assertIn('uid: canter-prometheus', datasource)
        self.assertIn('path: /etc/grafana/dashboards', provider)
        self.assertIn('allowUiUpdates: false', provider)

    def test_dashboard_is_valid_json_and_uses_provisioned_metrics(self):
        dashboard = json.loads((MONITORING / 'canter-overview.json').read_text())
        self.assertEqual(dashboard['uid'], 'canter-overview')
        self.assertFalse(dashboard['editable'])
        ids = [panel['id'] for panel in dashboard['panels']]
        self.assertEqual(len(ids), len(set(ids)))
        expressions = [target['expr'] for panel in dashboard['panels'] for target in panel['targets']]
        self.assertTrue(any('canter_http_requests_total' in expression for expression in expressions))
        self.assertTrue(any('canter_http_request_duration_seconds_bucket' in expression for expression in expressions))
        self.assertTrue(any('process_resident_memory_bytes' in expression for expression in expressions))
        self.assertTrue(any('node_memory_MemAvailable_bytes' in expression for expression in expressions))
        for panel in dashboard['panels']:
            self.assertEqual(panel['datasource']['uid'], 'canter-prometheus')


if __name__ == '__main__':
    unittest.main()

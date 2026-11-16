import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    'canter_ci_evidence', Path(__file__).with_name('canter-ci-evidence.py'))
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


class MeasurementTests(unittest.TestCase):
    def test_kernel_counters_and_interval_math(self):
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            (base / 'net').mkdir()
            (base / 'stat').write_text('cpu 100 20 30 400 50 6 4 90 25 5\n')
            (base / 'meminfo').write_text('MemTotal: 1024 kB\nMemAvailable: 256 kB\n')
            (base / 'vmstat').write_text('oom_kill 9\n')
            (base / 'net/route').write_text(
                'Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n'
                'ens4 00000000 0100000A 0003 0 0 200 00000000 0 0 0\n'
                'ens3 00000000 0100000A 0003 0 0 100 00000000 0 0 0\n')
            (base / 'net/dev').write_text(
                'ens3: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n'
                'ens4: 99999 0 0 0 0 0 0 0 99999 0 0 0 0 0 0 0\n')
            first = evidence.read_vm_counters(root)
            self.assertEqual(first['cpu_total_ticks'], 700)
            self.assertEqual(first['cpu_busy_ticks'], 160)
            self.assertEqual(first['network_interface'], 'ens3')
            (base / 'stat').write_text('cpu 140 30 70 900 100 8 12 140 99 11\n')
            (base / 'vmstat').write_text('oom_kill 11\n')
            (base / 'net/dev').write_text(
                'ens3: 1300 0 0 0 0 0 0 0 2600 0 0 0 0 0 0 0\n')
            current = evidence.read_vm_counters(root)
            self.assertEqual(current['cpu_total_ticks'], 1400)
            self.assertEqual(current['cpu_busy_ticks'], 260)
            metric = evidence.make_vm_metric(current, first, first, '2026-12-01T00:00:10Z')
            self.assertAlmostEqual(metric['cpuPercent'], 100 / 7)
            self.assertEqual(metric['memoryBytes'], 786432)
            self.assertEqual(metric['memoryLimitBytes'], 1048576)
            self.assertEqual(metric['networkRxBytes'], 300)
            self.assertEqual(metric['networkTxBytes'], 600)
            self.assertEqual(metric['oomKills'], 2)

    def test_missing_reset_and_changed_interface_stay_unknown(self):
        self.assertEqual(evidence.make_vm_metric({}, None, None, 'now'), {'at': 'now'})
        baseline = {'network_interface': 'ens3', 'network_rx_bytes': 50,
                    'network_tx_bytes': 60, 'oom_kills': 3,
                    'cpu_total_ticks': 100, 'cpu_busy_ticks': 20}
        current = {'network_interface': 'ens4', 'network_rx_bytes': 90,
                   'network_tx_bytes': 95, 'oom_kills': 1,
                   'cpu_total_ticks': 90, 'cpu_busy_ticks': 25,
                   'memory_total_bytes': 100, 'memory_available_bytes': 101}
        self.assertEqual(evidence.make_vm_metric(current, baseline, baseline, 'now'), {'at': 'now'})
        current['network_interface'] = 'ens3'
        current['network_rx_bytes'] = 40
        metric = evidence.make_vm_metric(current, baseline, baseline, 'now')
        self.assertNotIn('networkRxBytes', metric)
        self.assertEqual(metric['networkTxBytes'], 35)
        with tempfile.TemporaryDirectory() as missing:
            self.assertEqual(evidence.read_vm_counters(missing), {})

    def test_zero_cpu_is_observed_but_first_cpu_is_unknown(self):
        first = {'cpu_total_ticks': 100, 'cpu_busy_ticks': 20}
        later = {'cpu_total_ticks': 200, 'cpu_busy_ticks': 20}
        self.assertNotIn('cpuPercent', evidence.make_vm_metric(first, None, first, 'now'))
        self.assertEqual(evidence.make_vm_metric(later, first, first, 'now')['cpuPercent'], 0)
        later['cpu_busy_ticks'] = 120
        self.assertEqual(evidence.make_vm_metric(later, first, first, 'now')['cpuPercent'], 100)


class JUnitTests(unittest.TestCase):
    def test_reported_status_duration_and_duplicate_names(self):
        rows = evidence.parse_junit(b'''<testsuites><testsuite name="suite">
          <testcase name="same" time="0.0005"/>
          <testcase name="same"><failure message="failed">trace</failure></testcase>
          <testcase name="error" time="1.25"><error/></testcase>
          <testcase name="skip" status="notrun"/>
        </testsuite></testsuites>''')
        self.assertEqual([row['status'] for row in rows], ['passed', 'failed', 'error', 'skipped'])
        self.assertEqual(rows[0]['durationMs'], 1)
        self.assertNotIn('durationMs', rows[1])
        self.assertEqual(rows[2]['durationMs'], 1250)
        self.assertEqual(rows[1]['message'], 'failed\ntrace')
        self.assertNotEqual(rows[0]['id'], rows[1]['id'])
        self.assertEqual(rows, evidence.parse_junit(b'''<testsuites><testsuite name="suite">
          <testcase name="same" time="0.0005"/>
          <testcase name="same"><failure message="failed">trace</failure></testcase>
          <testcase name="error" time="1.25"><error/></testcase>
          <testcase name="skip" status="notrun"/>
        </testsuite></testsuites>'''))

    def test_namespace_nested_suites_and_output_are_not_extra_tests(self):
        rows = evidence.parse_junit(b'''<testsuites xmlns="urn:junit"><testsuite name="outer">
          <testsuite name="inner"><testcase name="one" classname="class" time="1e-3"/></testsuite>
          <system-out><testcase name="not-a-case"/></system-out>
        </testsuite></testsuites>''')
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]['suite'], 'outer/inner/class')
        self.assertEqual(rows[0]['durationMs'], 1)
        self.assertEqual(evidence.parse_junit(b'<testsuite/>'), [])

    def test_unsafe_ambiguous_and_oversized_reports_fail(self):
        for raw in (
                b'<!DOCTYPE x [<!ENTITY secret SYSTEM "file:///etc/passwd">]><testsuite/>',
                b'<testsuite><testcase name="x" time="NaN"/></testsuite>',
                b'<testsuite><testcase name="x" time="-1"/></testsuite>',
                b'<testsuite><testcase name="x" time="21600.001"/></testsuite>',
                b'<testsuite><testcase name="x" status="maybe"/></testsuite>',
                b'<testsuite><testcase/></testsuite>',
                b'<not-junit/>', b'broken', b'\xff',
                b'x' * (evidence.JUNIT_MAX_BYTES + 1)):
            with self.subTest(raw=raw[:100]):
                with self.assertRaises(evidence.EvidenceError):
                    evidence.parse_junit(raw)
        too_many = b'<testsuite>' + b'<testcase name="x"/>' * 1001 + b'</testsuite>'
        with self.assertRaises(evidence.EvidenceError):
            evidence.parse_junit(too_many)
        too_deep = b'<testsuite>' * 66 + b'<testcase name="x"/>' + b'</testsuite>' * 66
        with self.assertRaises(evidence.EvidenceError):
            evidence.parse_junit(too_deep)

    def test_long_failure_is_explicitly_truncated_in_utf8(self):
        raw = ('<testsuite><testcase name="x"><failure>' + '\u96ea' * 4000
               + '</failure></testcase></testsuite>').encode('utf-8')
        row = evidence.parse_junit(raw)[0]
        self.assertLessEqual(len(row['message'].encode('utf-8')), 8192)
        self.assertTrue(row['message'].endswith('\n[truncated]'))


class PacketTests(unittest.TestCase):
    def test_partition_preserves_every_utf8_case_and_wire_limits(self):
        tests = [{'id': format(i, '064x'), 'name': 'case', 'suite': '',
                  'status': 'failed', 'message': '\u96ea' * 2700}
                 for i in range(70)]
        packets = evidence.make_test_batches(tests)
        self.assertGreater(len(packets), 1)
        actual = []
        ids = set()
        for packet in packets:
            self.assertLessEqual(len(packet), 262144)
            decoded = json.loads(packet)
            self.assertEqual(decoded['schemaVersion'], 1)
            self.assertEqual(decoded['metrics'], [])
            self.assertRegex(decoded['batchId'], r'^[0-9a-f]{64}$')
            self.assertNotIn(decoded['batchId'], ids)
            ids.add(decoded['batchId'])
            actual.extend(decoded['tests'])
        self.assertEqual(actual, tests)
        self.assertEqual(evidence.make_test_batches([]), [])

    def test_exact_request_boundary_and_no_nonfinite_numeric_values(self):
        batch_id = 'a' * 64
        empty = {'id': 'b' * 64, 'name': 'n', 'suite': '', 'status': 'passed', 'message': ''}
        encoded = evidence.encode_evidence_batch([], [empty], batch_id)
        row = dict(empty, message='x' * (262144-len(encoded)))
        self.assertEqual(len(evidence.encode_evidence_batch([], [row], batch_id)), 262144)
        self.assertEqual(len(evidence.make_test_batches([row])[0]), 262144)
        row['message'] += 'x'
        with self.assertRaises(evidence.EvidenceError):
            evidence.encode_evidence_batch([], [row], batch_id)
        with self.assertRaises(evidence.EvidenceError):
            evidence.make_test_batches([row])
        with self.assertRaises(evidence.EvidenceError):
            evidence.encode_evidence_batch([{'at': 'now', 'cpuPercent': float('nan')}], [], batch_id)

    def test_explicit_reports_dedupe_identical_files_and_reject_special_files(self):
        with tempfile.TemporaryDirectory() as root:
            first = Path(root) / 'first.xml'
            second = Path(root) / 'second.xml'
            first.write_bytes(b'<testsuite><testcase name="one"/></testsuite>')
            second.write_bytes(b'<testsuite><testcase name="two" time="0"/></testsuite>')
            rows = evidence.read_junit_reports([first, first, second])
            self.assertEqual([row['name'] for row in rows], ['one', 'two'])
            self.assertNotIn('durationMs', rows[0])
            self.assertEqual(rows[1]['durationMs'], 0)
            with self.assertRaises(evidence.EvidenceError):
                evidence.read_junit_reports([root])
            with self.assertRaises(evidence.EvidenceError):
                evidence.read_junit_reports([first] * 65)
            second.write_bytes(b' ' * evidence.JUNIT_MAX_BYTES)
            with self.assertRaises(evidence.EvidenceError):
                evidence.read_junit_reports([first, second])


class SamplingScheduleTests(unittest.TestCase):
    def test_post_latency_does_not_shift_interval_or_extend_deadline(self):
        clock = [0.0]
        waits = []

        class Stop:
            def is_set(self):
                return False

            def wait(self, delay):
                waits.append(delay)
                clock[0] += delay
                return False

        original = evidence.monotonic, evidence.read_vm_counters, evidence.utc_timestamp
        frames = iter([
            {'cpu_total_ticks': 200, 'cpu_busy_ticks': 40},
            {'cpu_total_ticks': 300, 'cpu_busy_ticks': 100},
        ])
        evidence.monotonic = lambda: clock[0]
        evidence.read_vm_counters = lambda: next(frames)
        evidence.utc_timestamp = lambda: str(clock[0])
        try:
            baseline = {'cpu_total_ticks': 100, 'cpu_busy_ticks': 20,
                        'memory_total_bytes': 100, 'memory_available_bytes': 60}
            stream = evidence.iter_vm_metrics(baseline, '0.0', Stop(), 25)
            first = next(stream)
            self.assertNotIn('cpuPercent', first)
            self.assertEqual(first['memoryBytes'], 40)
            clock[0] += 3
            self.assertEqual(next(stream), {'at': '10.0', 'cpuPercent': 20.0})
            clock[0] += 3
            self.assertEqual(next(stream), {'at': '20.0', 'cpuPercent': 60.0})
            clock[0] += 3
            with self.assertRaises(StopIteration):
                next(stream)
            self.assertEqual(waits, [7, 7, 2])
        finally:
            evidence.monotonic, evidence.read_vm_counters, evidence.utc_timestamp = original


if __name__ == '__main__':
    unittest.main()

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ciJobActive, ciJobPhase, ciJobStateText, ciRunnerEvidenceRows, ciSizeLabel, ciStateText, featureStateText,
  filterCIFJobs, formatDurationMs, formatDurationSeconds, formatMoneyMicros, formatReportedDurationMs,
  formatRatePerMinute, freePeriodText, metricText, monthlyQuotaText, parseCount,
  parseSpendCapCents, poolStateText, runsOnLabel, safeGitHubURL, shortSha, sizeSummary,
  spendCapText, ciLogCollectionNotice,
} from '../src/lib/ci.ts';
import { quicknavItems } from '../src/lib/quicknav-items.ts';

const sizes = [
  { id: 'linux-x64-2', label: 'canter-2vcpu-ubuntu-2404', vcpus: 2, memoryMiB: 8192, microsPerMinute: 2000, blacksmithMicrosPerMinute: 4000 },
  { id: 'linux-x64-4', label: 'canter-4vcpu-ubuntu-2404', vcpus: 4, memoryMiB: 16384, microsPerMinute: 4000, blacksmithMicrosPerMinute: 8000 },
];
const job = (over = {}) => ({ id: 'job_1', repository: 'acme/api', name: 'build', workflowName: 'ci', ref: 'refs/heads/main', headSha: 'abcdef1234567890', githubJobId: 1, githubRunId: 2, githubURL: 'https://github.com/acme/api/actions/runs/2/job/1', status: 'completed', conclusion: 'success', size: 'linux-x64-4', createdAt: '2026-11-08T00:00:00Z', elapsedSeconds: 3900, billableSeconds: 300, costMicros: 20000, queueReason: '', ...over });

test('safeGitHubURL allows only plain https github.com links', () => {
  assert.equal(safeGitHubURL('https://github.com/acme/api/actions/runs/2/job/1'), 'https://github.com/acme/api/actions/runs/2/job/1');
  assert.equal(safeGitHubURL('https://github.com:443/acme/api'), 'https://github.com/acme/api');
  for (const bad of ['javascript:alert(1)', 'data:text/html,x', 'http://github.com/a/b', 'https://evil.com/a', 'https://github.com.evil.com/a', 'https://sub.github.com/a', 'https://user:pw@github.com/a', 'https://github.com:444/a', '', null, undefined, 'not a url']) {
    assert.equal(safeGitHubURL(bad), null, String(bad));
  }
});

test('money and rate formatting comes from server micros values', () => {
  assert.equal(formatMoneyMicros(20000, 'USD'), '$0.02');
  assert.equal(formatMoneyMicros(2000, 'USD'), '$0.002');
  assert.equal(formatMoneyMicros(1234567, 'USD'), '$1.234567');
  assert.equal(formatMoneyMicros(0, 'USD'), '$0');
  assert.equal(formatRatePerMinute(2000, 'USD'), '$0.002/min');
  assert.equal(formatRatePerMinute(4000, 'USD'), '$0.004/min');
  assert.equal(formatRatePerMinute(1234, 'USD'), '$0.001234/min');
  assert.equal(formatMoneyMicros(2000, 'EUR'), 'EUR 0.002');
});

test('free period and quota copy derive from server fields, never a monthly quota', () => {
  assert.equal(freePeriodText(3600), '1 hour');
  assert.equal(freePeriodText(7200), '2 hours');
  assert.equal(freePeriodText(1800), '30 minutes');
  assert.equal(freePeriodText(90), '90 seconds');
  assert.match(monthlyQuotaText(null), /no monthly minute quota/i);
  assert.equal(monthlyQuotaText(5000), 'Monthly minute limit: 5000.');
});

test('job state, phase and active helpers', () => {
  assert.equal(ciJobStateText(job()), 'Succeeded');
  assert.equal(ciJobStateText(job({ status: 'completed', conclusion: 'failure' })), 'Failed');
  assert.equal(ciJobStateText(job({ status: 'in_progress', conclusion: undefined })), 'Running');
  assert.equal(ciJobStateText(job({ status: 'queued', conclusion: undefined })), 'Queued');
  assert.equal(ciJobPhase(job()), 'succeeded');
  assert.equal(ciJobPhase(job({ status: 'completed', conclusion: 'timed_out' })), 'failed');
  assert.equal(ciJobPhase(job({ status: 'in_progress', conclusion: undefined })), 'running');
  assert.equal(ciJobPhase(job({ status: 'queued', conclusion: undefined })), 'drafted');
  assert.equal(ciJobActive(job()), false);
  assert.equal(ciJobActive(job({ status: 'in_progress', conclusion: undefined })), true);
  assert.equal(ciJobActive(job({ status: 'queued', conclusion: undefined })), true);
  assert.equal(ciJobActive(job({ status: 'completed', conclusion: 'cancelled' })), false);
});

test('recent job filter matches repository, name, workflow and ref only', () => {
  const jobs = [job(), job({ id: 'job_2', repository: 'acme/web', name: 'deploy', workflowName: 'release', ref: 'refs/heads/feature' })];
  assert.equal(filterCIFJobs(jobs, '').length, 2);
  assert.equal(filterCIFJobs(jobs, '  ').length, 2);
  assert.deepEqual(filterCIFJobs(jobs, 'ACME/WEB').map(j => j.id), ['job_2']);
  assert.deepEqual(filterCIFJobs(jobs, 'deploy').map(j => j.id), ['job_2']);
  assert.deepEqual(filterCIFJobs(jobs, 'feature').map(j => j.id), ['job_2']);
  assert.equal(filterCIFJobs(jobs, 'zzz').length, 0);
});

test('spend cap accepts whole cents within 1..100000 only', () => {
  assert.equal(parseSpendCapCents('10'), 1000);
  assert.equal(parseSpendCapCents('10.50'), 1050);
  assert.equal(parseSpendCapCents('0.01'), 1);
  assert.equal(parseSpendCapCents('1000'), 100000);
  for (const bad of ['0', '0.001', '1000.01', '-5', 'abc', '', '1e3', '10.555']) assert.equal(parseSpendCapCents(bad), null, bad);
  assert.equal(spendCapText(1000), '10.00');
  assert.equal(spendCapText(1), '0.01');
});

test('retain minutes and misc helpers', () => {
  assert.equal(parseCount('0'), 0);
  assert.equal(parseCount('30'), 30);
  for (const bad of ['-1', '1.5', 'abc', '']) assert.equal(parseCount(bad), null, bad);
  assert.equal(metricText(null, String), 'unavailable');
  assert.equal(metricText(undefined, String), 'unavailable');
  assert.equal(metricText(0, String), '0');
  assert.equal(metricText(42.5, v => `${v}%`), '42.5%');
  assert.equal(featureStateText('available'), 'Available');
  assert.equal(featureStateText('preview'), 'Preview');
  assert.equal(featureStateText('not_configured'), 'Not configured');
  assert.equal(featureStateText('something_new'), 'Something new');
  assert.equal(ciStateText('in_progress'), 'Running');
  assert.equal(ciStateText('timed_out'), 'Timed out');
  assert.equal(poolStateText({ ready: true, status: 'ready', message: '' }), 'Ready');
  assert.equal(poolStateText({ ready: false, status: 'paused', message: 'Pool paused' }), 'Paused');
});

test('size helpers use backend-provided labels and specs', () => {
  assert.equal(runsOnLabel(sizes[0]), 'canter-2vcpu-ubuntu-2404');
  assert.equal(ciSizeLabel(sizes, 'linux-x64-4'), 'canter-4vcpu-ubuntu-2404');
  assert.equal(ciSizeLabel(sizes, 'canter-2vcpu-ubuntu-2404'), 'canter-2vcpu-ubuntu-2404');
  assert.equal(ciSizeLabel(sizes, 'unknown'), 'unknown');
  assert.equal(sizeSummary(sizes[0]), '2 vCPU · 8 GiB');
  assert.equal(sizeSummary(sizes[1]), '4 vCPU · 16 GiB');
});

test('duration and sha helpers', () => {
  assert.equal(formatDurationSeconds(0), '0s');
  assert.equal(formatDurationSeconds(45), '45s');
  assert.equal(formatDurationSeconds(125), '2m 5s');
  assert.equal(formatDurationSeconds(3600), '1h');
  assert.equal(formatDurationSeconds(3725), '1h 2m');
  assert.equal(formatDurationMs(850), '850ms');
  assert.equal(formatDurationMs(1500), '2s');
  assert.equal(formatReportedDurationMs(null), 'Duration unavailable');
  assert.equal(formatReportedDurationMs(undefined), 'Duration unavailable');
  assert.equal(formatReportedDurationMs(0), '0ms');
  assert.equal(formatReportedDurationMs(1500), '2s');
  assert.equal(shortSha('abcdef1234567890'), 'abcdef1…');
  assert.equal(shortSha('abc'), 'abc');
});

test('quicknav shows CI only when the product is enabled', () => {
  const catalog = [{ id: 'ci', name: 'CI', description: '', enabled: false, alwaysOn: false }];
  assert.deepEqual(quicknavItems(catalog).map(item => item.href), ['/app', '/app/products']);
  const enabled = quicknavItems([{ ...catalog[0], enabled: true }]);
  assert.deepEqual(enabled.map(item => item.href), ['/app', '/app/ci', '/app/products']);
  assert.equal(enabled[1].icon, 'changes');
  const ordered = quicknavItems([
    { id: 'sandboxes', name: 'Sandboxes', description: '', enabled: true, alwaysOn: false },
    { id: 'ci', name: 'CI', description: '', enabled: true, alwaysOn: false },
    { id: 'wrens', name: 'Wrens', description: '', enabled: true, alwaysOn: true },
  ]);
  assert.deepEqual(ordered.map(item => item.href), ['/app', '/app/sandboxes', '/app/ci', '/app/wrens', '/app/products']);
});

const diag = (over = {}) => ({
  schemaVersion: 1, runnerId: 'ci_runner_1', association: 'assigned', phase: 'running',
  createdAt: '2026-12-01T00:00:00Z', updatedAt: '2026-12-01T00:05:00Z',
  deleteRequested: false, createAttemptedAt: null, createSettled: null,
  setupDeadlineAt: null, hardDeadlineAt: null, executionStartedAt: null,
  executionStoppedAt: null, deadlineAt: null, stopRequestedAt: null,
  stopReason: null, billingStopAt: null, stoppedVerifiedAt: null,
  cleanupVerifiedAt: null, ...over,
});
const diagRow = (rows, label) => rows.find(row => row.label === label)?.value;

test('ciRunnerEvidenceRows returns no rows without diagnostics', () => {
  assert.deepEqual(ciRunnerEvidenceRows(null), []);
  assert.deepEqual(ciRunnerEvidenceRows(undefined), []);
});

test('ciRunnerEvidenceRows marks unrecorded evidence unavailable, never success', () => {
  const rows = ciRunnerEvidenceRows(diag());
  assert.equal(rows.length, 18);
  assert.equal(diagRow(rows, 'Runner'), 'ci_runner_1');
  assert.equal(diagRow(rows, 'Association'), 'Assigned runner');
  assert.equal(diagRow(rows, 'Phase'), 'Running');
  assert.equal(diagRow(rows, 'Created'), '2026-12-01T00:00:00.000Z');
  assert.equal(diagRow(rows, 'Updated'), '2026-12-01T00:05:00.000Z');
  assert.equal(diagRow(rows, 'Creation attempted'), 'Unavailable');
  assert.equal(diagRow(rows, 'Creation settled'), 'Unavailable');
  assert.equal(diagRow(rows, 'Delete request'), 'Not requested');
  assert.equal(diagRow(rows, 'Setup deadline'), 'Unavailable');
  assert.equal(diagRow(rows, 'Hard deadline'), 'Unavailable');
  assert.equal(diagRow(rows, 'Execution started'), 'Unavailable');
  assert.equal(diagRow(rows, 'Execution stop (settled)'), 'Unavailable');
  assert.equal(diagRow(rows, 'Execution deadline'), 'Unavailable');
  assert.equal(diagRow(rows, 'Stop requested'), 'Unavailable');
  assert.equal(diagRow(rows, 'Stop reason'), 'Not recorded');
  assert.equal(diagRow(rows, 'Billing cutoff'), 'Unavailable');
  assert.equal(diagRow(rows, 'Stop verified'), 'Unavailable');
  assert.equal(diagRow(rows, 'Cleanup verified'), 'Unavailable');
});

test('ciRunnerEvidenceRows distinguishes stop intent from verified proof', () => {
  const rows = ciRunnerEvidenceRows(diag({
    phase: 'deleting', deleteRequested: true,
    stopRequestedAt: '2026-12-01T01:00:00Z', stopReason: 'cancelled',
    billingStopAt: '2026-12-01T01:00:00Z',
    stoppedVerifiedAt: null, cleanupVerifiedAt: null,
  }));
  assert.equal(diagRow(rows, 'Phase'), 'Deleting');
  assert.equal(diagRow(rows, 'Delete request'), 'Requested');
  assert.equal(diagRow(rows, 'Stop requested'), '2026-12-01T01:00:00.000Z');
  assert.equal(diagRow(rows, 'Stop reason'), 'Cancelled');
  assert.equal(diagRow(rows, 'Billing cutoff'), '2026-12-01T01:00:00.000Z');
  assert.equal(diagRow(rows, 'Stop verified'), 'Unavailable');
  assert.equal(diagRow(rows, 'Cleanup verified'), 'Unavailable');
});

test('ciRunnerEvidenceRows renders stored proof and settled states exactly', () => {
  const rows = ciRunnerEvidenceRows(diag({
    association: 'provisioning_attempt',
    createAttemptedAt: '2026-12-01T00:01:00Z', createSettled: false,
    setupDeadlineAt: '2026-12-01T00:16:00Z', hardDeadlineAt: '2026-12-02T06:16:00Z',
    executionStartedAt: '2026-12-01T00:02:00Z', executionStoppedAt: '2026-12-01T00:30:00Z',
    deadlineAt: '2026-12-01T02:02:00Z', stoppedVerifiedAt: '2026-12-01T02:00:00Z',
    cleanupVerifiedAt: '2026-12-01T02:05:00Z',
  }));
  assert.equal(diagRow(rows, 'Association'), 'Provisioning attempt');
  assert.equal(diagRow(rows, 'Creation attempted'), '2026-12-01T00:01:00.000Z');
  assert.equal(diagRow(rows, 'Creation settled'), 'Not settled');
  assert.equal(diagRow(rows, 'Setup deadline'), '2026-12-01T00:16:00.000Z');
  assert.equal(diagRow(rows, 'Hard deadline'), '2026-12-02T06:16:00.000Z');
  assert.equal(diagRow(rows, 'Execution started'), '2026-12-01T00:02:00.000Z');
  assert.equal(diagRow(rows, 'Execution stop (settled)'), '2026-12-01T00:30:00.000Z');
  assert.equal(diagRow(rows, 'Execution deadline'), '2026-12-01T02:02:00.000Z');
  assert.equal(diagRow(rows, 'Stop verified'), '2026-12-01T02:00:00.000Z');
  assert.equal(diagRow(rows, 'Cleanup verified'), '2026-12-01T02:05:00.000Z');
  assert.equal(diagRow(ciRunnerEvidenceRows(diag({ createSettled: true })), 'Creation settled'), 'Settled');
  assert.equal(diagRow(ciRunnerEvidenceRows(diag({ association: 'mystery' })), 'Association'), 'Unknown');
});

test('ciRunnerEvidenceRows treats missing or malformed timestamps as unavailable', () => {
  const rows = ciRunnerEvidenceRows(diag({ createdAt: 'not-a-date', updatedAt: '', deadlineAt: '2026-13-40T99:99:99Z' }));
  assert.equal(diagRow(rows, 'Created'), 'Unavailable');
  assert.equal(diagRow(rows, 'Updated'), 'Unavailable');
  assert.equal(diagRow(rows, 'Execution deadline'), 'Unavailable');
});

test('ciLogCollectionNotice covers every collection state without mutation', () => {
  assert.equal(ciLogCollectionNotice(undefined), null);
  assert.equal(ciLogCollectionNotice(null), null);
  assert.equal(ciLogCollectionNotice({ state: 'collecting', truncated: false, collectedAt: null }), 'Fetching completed job logs from GitHub.');
  assert.equal(ciLogCollectionNotice({ state: 'unavailable', truncated: false, collectedAt: null }), 'GitHub job logs are unavailable; collection will retry while this job remains eligible.');
  assert.equal(ciLogCollectionNotice({ state: 'complete', truncated: false, collectedAt: '2026-12-01T00:00:00Z' }), 'GitHub Actions logs collected after job completion.');
  assert.equal(ciLogCollectionNotice({ state: 'complete', truncated: true, collectedAt: '2026-12-01T00:00:00Z' }), 'GitHub Actions logs collected after job completion. Output is limited to the first 1 MiB.');
  assert.equal(ciLogCollectionNotice({ state: 'complete', truncated: false, collectedAt: null }), 'GitHub log collection proof is unavailable.');
  assert.equal(ciLogCollectionNotice({ state: 'complete', truncated: false, collectedAt: '' }), 'GitHub log collection proof is unavailable.');
  assert.equal(ciLogCollectionNotice({ state: 'complete', truncated: false, collectedAt: 'not-a-date' }), 'GitHub log collection proof is unavailable.');
  assert.equal(ciLogCollectionNotice({ state: 'complete', truncated: false, collectedAt: '2026-13-40T99:99:99Z' }), 'GitHub log collection proof is unavailable.');
  assert.equal(ciLogCollectionNotice({ state: 'mystery', truncated: false, collectedAt: '2026-12-01T00:00:00Z' }), 'GitHub log collection status is unavailable.');
  assert.equal(ciLogCollectionNotice({ state: '', truncated: false }), 'GitHub log collection status is unavailable.');
});

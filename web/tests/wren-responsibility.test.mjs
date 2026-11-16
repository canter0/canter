import test from 'node:test';
import assert from 'node:assert/strict';
import { localResponsibilityTime, presetCron, responsibilityTimeCandidates, scheduleMode } from '../src/lib/wren-responsibility.ts';

test('daily and weekly controls produce timezone-independent five-field schedules', () => {
  assert.equal(presetCron('daily', '09:30', '1'), '30 9 * * *');
  assert.equal(presetCron('weekdays', '08:05', '1'), '5 8 * * 1-5');
  assert.equal(presetCron('weekly', '17:15', '5'), '15 17 * * 5');
  assert.throws(() => presetCron('weekly', '25:00', '1'));
  assert.equal(scheduleMode({ kind: 'cron', cron: '0 9 * * 1-5' }), 'weekdays');
  assert.equal(scheduleMode({ kind: 'cron', cron: '*/15 8-17 * * 1-5' }), 'custom');
});
test('one-time schedules use the selected timezone rather than the browser timezone', () => {
  assert.deepEqual(responsibilityTimeCandidates('2026-10-26T09:00:00', 'America/Toronto'), ['2026-10-26T13:00:00.000Z']);
  assert.equal(localResponsibilityTime('2026-10-26T13:00:00Z', 'America/Toronto'), '2026-10-26T09:00:00');
  assert.deepEqual(responsibilityTimeCandidates('2026-10-26T09:00', 'Asia/Kathmandu'), ['2026-10-26T03:15:00.000Z']);
});
test('DST gaps are rejected and repeated wall times offer both real occurrences', () => {
  assert.deepEqual(responsibilityTimeCandidates('2026-03-08T02:30', 'America/Toronto'), []);
  assert.deepEqual(responsibilityTimeCandidates('2026-11-01T01:30', 'America/Toronto'), ['2026-11-01T05:30:00.000Z', '2026-11-01T06:30:00.000Z']);
  assert.throws(() => responsibilityTimeCandidates('2026-02-30T09:00', 'UTC'));
  assert.throws(() => responsibilityTimeCandidates('2026-10-26T09:00', 'Not/AZone'));
});

import test from 'node:test';
import assert from 'node:assert/strict';
import { desktopPoint, activeJob, jobLabel, compareWorkTimestamps } from '../src/lib/wren-work-state.ts';

test('desktop input scales a displayed screenshot to its actual natural pixels', () => {
  const image = { left: 100, top: 50, width: 640, height: 400, naturalWidth: 1280, naturalHeight: 800 };
  assert.deepEqual(desktopPoint(image, 420, 250), { x: 640, y: 400 });
  assert.deepEqual(desktopPoint({ ...image, width: 320, height: 200 }, 420, 250), { x: 1279, y: 799 });
  assert.deepEqual(desktopPoint(image, 90, 40), { x: 0, y: 0 });
  assert.deepEqual(desktopPoint(image, 900, 600), { x: 1279, y: 799 });
});
test('unloaded or invalid screenshot geometry never produces an input coordinate', () => {
  const image = { left: 0, top: 0, width: 400, height: 250, naturalWidth: 1280, naturalHeight: 800 };
  assert.equal(desktopPoint({ ...image, naturalWidth: 0 }, 20, 30), null);
  assert.equal(desktopPoint({ ...image, height: 0 }, 20, 30), null);
  assert.equal(desktopPoint(image, Number.NaN, 30), null);
});
test('worker polling follows execution and pending publication rather than implying constant activity', () => {
  assert.equal(activeJob({ status: 'running' }), true);
  assert.equal(activeJob({ status: 'queued' }), true);
  assert.equal(activeJob({ status: 'completed', publishedAt: '2026-10-25T00:00:00Z' }), false);
  assert.equal(activeJob({ status: 'completed', returnSuppressed: false }), true);
  assert.equal(activeJob({ status: 'cancelled', returnSuppressed: true }), false);
  assert.equal(jobLabel({ status: 'queued', executorStarted: false }), 'Queued');
});

test('control snapshots retain sub-millisecond ordering instead of reviving an old lease', () => {
  assert.equal(compareWorkTimestamps('2026-10-25T12:00:00.000123Z', '2026-10-25T12:00:00.000124Z'), -1);
  assert.equal(compareWorkTimestamps('2026-10-25T12:00:00.000125Z', '2026-10-25T12:00:00.000124Z'), 1);
  assert.equal(compareWorkTimestamps('2026-10-25T12:00:00.1Z', '2026-10-25T12:00:00.100000Z'), 0);
});

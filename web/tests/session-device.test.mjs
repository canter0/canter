import test from 'node:test';
import assert from 'node:assert/strict';
import { sessionDeviceLabel } from '../src/lib/session-device.ts';

test('session labels name the browser and device for common agents', () => {
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0'), 'Edge on Windows');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36'), 'Chrome on Mac');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0'), 'Firefox on Linux');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15'), 'Safari on Mac');
});

test('mobile agents resolve before desktop tokens inside the same string', () => {
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1'), 'Safari on iPhone');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36'), 'Chrome on Android');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) EdgiOS/131.0.0.0 Mobile/15E148 Safari/605.1.15'), 'Edge on iPhone');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1'), 'Safari on iPad');
});

test('unknown and partial agents fall back without inventing details', () => {
  assert.equal(sessionDeviceLabel(''), 'Browser session');
  assert.equal(sessionDeviceLabel('curl/8.7.1'), 'Browser session');
  assert.equal(sessionDeviceLabel('Mozilla/5.0 (X11; Linux x86_64) CustomAgent/1.0'), 'Browser on Linux');
  assert.equal(sessionDeviceLabel('SomeBot/2.0 (compatible; Windows NT 10.0)'), 'Browser on Windows');
});

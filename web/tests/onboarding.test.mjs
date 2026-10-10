import test from 'node:test';
import assert from 'node:assert/strict';
import { showOnboarding } from '../src/lib/onboarding.ts';

test('unused accounts reach onboarding through the ordinary dashboard route', () => {
  assert.equal(showOnboarding(false), true);
  assert.equal(showOnboarding(true), false);
  assert.equal(showOnboarding(undefined), false);
});

test('draft transitions bypass onboarding and explicit welcome links can revisit it', () => {
  assert.equal(showOnboarding(false, undefined, '1'), false);
  assert.equal(showOnboarding(true, '1'), true);
  assert.equal(showOnboarding(false, '0'), true);
  assert.equal(showOnboarding(true, '0'), false);
});

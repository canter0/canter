import test from 'node:test';
import assert from 'node:assert/strict';
import { authDestination } from '../src/lib/auth.ts';

test('auth destinations stay on the application after URL path normalization', () => {
  for (const destination of ['/.//attacker.example', '/%2e//attacker.example', '/account/.././/attacker.example']) {
    assert.equal(authDestination(destination), '/app', destination);
  }
});

test('auth destinations preserve ordinary local paths and reject direct network paths', () => {
  assert.equal(authDestination('/app/billing?plan=pro#details'), '/app/billing?plan=pro#details');
  assert.equal(authDestination('//attacker.example'), '/app');
});

test('signup welcomes preserve an explicit review destination and reject external redirects', () => {
  const welcome = '/app?welcome=1';
  assert.equal(authDestination('', welcome), welcome);
  assert.equal(authDestination('/approve/change/review-token', welcome), '/approve/change/review-token');
  assert.equal(authDestination('/.//attacker.example', welcome), welcome);
});

import { expiredSessionDestination } from '../src/lib/auth.ts';

test('expired sessions return to the live app path and never leave /app', () => {
  assert.equal(expiredSessionDestination('/app/wrens'), '/app/wrens');
  assert.equal(expiredSessionDestination('/app'), '/app');
  assert.equal(expiredSessionDestination('/app/conversations/conv_1?run=r_1#bottom'), '/app/conversations/conv_1');
  assert.equal(expiredSessionDestination('/join?invite=winv_secret'), '/app');
  assert.equal(expiredSessionDestination('/account/security'), '/app');
  assert.equal(expiredSessionDestination('//attacker.example'), '/app');
  assert.equal(expiredSessionDestination('https://attacker.example/app'), '/app');
  assert.equal(expiredSessionDestination('/app/.././/attacker.example'), '/app');
});

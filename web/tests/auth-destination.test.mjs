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

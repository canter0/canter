import test from 'node:test';
import assert from 'node:assert/strict';
import { bindAShortcuts } from '../src/lib/a-shortcuts.ts';
import { searchCommands } from '../src/lib/spotlight.ts';

function fixture() {
  const target = new EventTarget();
  const calls = [];
  let blocked = false;
  const cleanup = bindAShortcuts(target, key => {
    if (!['n', 'q', 'e', ' ', '1', '[', ']'].includes(key)) return false;
    calls.push(key);
    return true;
  }, () => blocked);
  const key = (value, options = {}, type = 'keydown') => {
    const event = new Event(type, { cancelable: true });
    Object.assign(event, { key: value, code: value === ' ' ? 'Space' : `Key${value.toUpperCase()}`, ...options });
    target.dispatchEvent(event);
    return event.defaultPrevented;
  };
  return { target, calls, key, cleanup, block: value => { blocked = value; } };
}

test('A is a held chord, not a sticky prefix, and unknown keys retain their behavior', () => {
  const f = fixture();
  assert.equal(f.key('n'), false);
  f.key('a'); f.key('a', {}, 'keyup'); f.key('n');
  assert.deepEqual(f.calls, []);
  f.key('a');
  assert.equal(f.key('z'), false);
  assert.equal(f.key('n'), true);
  assert.deepEqual(f.calls, ['n']);
});

test('repeats cannot toggle repeatedly or leak the consumed key into newly focused inputs', () => {
  const f = fixture();
  f.key('a'); f.key('q'); f.key('q', { repeat: true });
  assert.deepEqual(f.calls, ['q']);
  f.key('q', {}, 'keyup'); f.key('q');
  assert.deepEqual(f.calls, ['q', 'q']);
  f.key(' '); f.block(true);
  assert.equal(f.key(' ', { repeat: true }), true);
  f.key(' ', {}, 'keyup');
  assert.equal(f.key(' '), false);
});

test('typing and modal contexts never arm the chord and cancel an armed chord', () => {
  const f = fixture();
  f.block(true); assert.equal(f.key('a'), false);
  f.block(false); f.key('n');
  f.key('a'); f.block(true); assert.equal(f.key('q'), false);
  f.block(false); f.key('e');
  assert.deepEqual(f.calls, []);
});

test('browser modifiers, composition, and handled events cancel the chord', () => {
  for (const options of [{ metaKey: true }, { ctrlKey: true }, { altKey: true }, { shiftKey: true }, { isComposing: true }, { keyCode: 229 }]) {
    const f = fixture();
    f.key('a'); assert.equal(f.key('n', options), false); f.key('n');
    assert.deepEqual(f.calls, []);
  }
  const f = fixture();
  f.key('a');
  const event = new Event('keydown', { cancelable: true });
  Object.assign(event, { key: 'n' }); event.preventDefault(); f.target.dispatchEvent(event);
  f.key('q'); assert.deepEqual(f.calls, []);
});

test('focus loss, page exit and cleanup release held keys', () => {
  for (const event of ['blur', 'pagehide']) {
    const f = fixture(); f.key('a'); f.target.dispatchEvent(new Event(event)); f.key('n');
    assert.deepEqual(f.calls, []);
  }
  const f = fixture(); f.key('a'); f.cleanup(); f.key('n');
  assert.deepEqual(f.calls, []);
});

test('Space and tab shortcuts work and uppercase A from Caps Lock is accepted', () => {
  const f = fixture(); f.key('A');
  for (const key of [' ', 'e', '1', '[', ']']) { assert.equal(f.key(key), true); f.key(key, {}, 'keyup'); }
  assert.deepEqual(f.calls, [' ', 'e', '1', '[', ']']);
});

test('Spotlight searches commands by case-insensitive words without running them', () => {
  const commands = [{ id: 'panel', title: 'Expand right sidebar fully', key: 'e', run: () => assert.fail('Search must not run commands') }, { id: 'new', title: 'New conversation', key: 'n' }];
  assert.deepEqual(searchCommands(commands, ' SIDEBAR right '), [commands[0]]);
  assert.deepEqual(searchCommands(commands, 'xyz'), []);
  assert.deepEqual(searchCommands(commands, ''), commands);
});

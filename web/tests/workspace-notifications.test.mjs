import test from 'node:test';
import assert from 'node:assert/strict';
import { workspaceNotifications, notificationReceiptKey, parseNotificationReceipts, securityNotifications } from '../src/lib/workspace-notifications.ts';

const date = '2026-10-25T12:00:00Z';
function workspace(extra = {}) {
  return { workspace: { id: 'workspace_a' }, conversations: [], tasks: [], changes: [], initialDeployments: [], ...extra };
}

test('the inbox includes actual attention states and excludes normal ongoing/completed work', () => {
  const notifications = workspaceNotifications(workspace({
    conversations: [
      { id: 'failed', workspaceId: 'workspace_a', status: 'failed', title: 'Please check this', updatedAt: date },
      { id: 'running', workspaceId: 'workspace_a', status: 'running', title: 'Still working', updatedAt: date },
      { id: 'completed', workspaceId: 'workspace_a', status: 'completed', title: 'Finished', updatedAt: date },
    ],
    changes: [
      { id: 'review', phase: 'drafted', system: 'site', summary: 'Review a change', digest: 'digest', createdAt: date },
      { id: 'done', phase: 'committed', system: 'site', summary: 'Done', digest: 'digest', createdAt: date },
      { id: 'execution', phase: 'authorized', executionPhase: 'failed', system: 'site', summary: 'Execution failed', digest: 'digest', createdAt: date },
    ],
  }));
  assert.equal(notifications.length, 3);
  assert.deepEqual(notifications.map(item => item.kind).sort(), ['failure', 'failure', 'review']);
  assert.ok(notifications.some(item => item.href === '/app/conversations/failed'));
  assert.ok(notifications.some(item => item.href === '/app/changes/review?system=site'));
});

test('conversation and task notifications cannot cross a workspace boundary', () => {
  const notifications = workspaceNotifications(workspace({
    conversations: [{ id: 'foreign', workspaceId: 'workspace_b', status: 'failed', title: 'Private', updatedAt: date }],
    tasks: [{ id: 'foreign-task', workspaceId: 'workspace_b', status: 'failed', prompt: 'Private task', updatedAt: date }],
  }));
  assert.deepEqual(notifications, []);
});

test('read receipts track a specific state/version, so a new failure is unread', () => {
  const failure = { id: 'session', workspaceId: 'workspace_a', status: 'failed', title: 'Session', updatedAt: date };
  const [first] = workspaceNotifications(workspace({ conversations: [failure] }));
  const receipts = parseNotificationReceipts(JSON.stringify({ [first.id]: true }));
  assert.equal(receipts[first.id], true);
  const [second] = workspaceNotifications(workspace({ conversations: [{ ...failure, updatedAt: '2026-10-25T13:00:00Z' }] }));
  assert.equal(receipts[second.id], undefined);
  assert.deepEqual(workspaceNotifications(workspace({ conversations: [{ ...failure, status: 'completed' }] })), []);
});

test('receipt storage is scoped to both account and workspace and safely handles invalid data', () => {
  assert.notEqual(notificationReceiptKey('a', 'one'), notificationReceiptKey('b', 'one'));
  assert.notEqual(notificationReceiptKey('a', 'one'), notificationReceiptKey('a', 'two'));
  for (const raw of ['', 'not json', 'null', '[]', 'false']) assert.deepEqual(parseNotificationReceipts(raw), {});
  assert.deepEqual(parseNotificationReceipts('{"valid":true,"false":false,"string":"true"}'), { valid: true });
  const many = Object.fromEntries(Array.from({ length: 450 }, (_, i) => [`item${i}`, true]));
  assert.equal(Object.keys(parseNotificationReceipts(JSON.stringify(many))).length, 400);
});

test('notification destinations are encoded and repeated records do not duplicate notifications', () => {
  const deployment = { id: 'deployment/a', system: 'site', phase: 'drafted', summary: 'Ready', digest: 'one', createdAt: date };
  const notifications = workspaceNotifications(workspace({ initialDeployments: [deployment, deployment] }));
  assert.equal(notifications.length, 1);
  assert.equal(notifications[0].href, '/app/changes/initial/deployment%2Fa');
});

test('pending organization invites appear as invite notifications with the target workspace', () => {
  const notifications = workspaceNotifications(workspace({
    invites: [
      { id: 'winvite_a', workspaceId: 'wrk_team', workspaceName: 'Team Org', inviterEmail: 'owner@example.com', role: 'operator', expiresAt: date },
      { id: 'winvite_b', workspaceId: 'wrk_default', workspaceName: 'default', inviterEmail: '', role: 'viewer', expiresAt: '2026-10-26T12:00:00Z' },
    ],
  }));
  assert.deepEqual(notifications.map(item => item.kind), ['invite', 'invite']);
  const first = notifications.find(item => item.id === 'invite:winvite_a');
  const second = notifications.find(item => item.id === 'invite:winvite_b');
  assert.equal(first.inviteId, 'winvite_a');
  assert.equal(first.workspaceId, 'wrk_team');
  assert.equal(first.title, 'owner@example.com invited you to Team Org');
  assert.equal(first.detail, 'Join as operator.');
  assert.equal(second.title, 'Someone invited you to Personal');
});

test('security notifications include only listed actions and exclude the viewer', () => {
  const events = [
    { id: 'e1', action: 'agent.authorized', subject: 'inst_1', occurredAt: '2026-10-25T12:00:00Z', actor: { kind: 'human', id: 'usr_other', displayName: 'ana@example.com' } },
    { id: 'e2', action: 'agent.revoked', subject: 'inst_2', occurredAt: '2026-10-25T11:00:00Z', actor: { kind: 'human', id: 'usr_me', displayName: 'me@example.com' } },
    { id: 'e3', action: 'member.left', subject: 'usr_x', occurredAt: '2026-10-25T10:00:00Z', actor: { kind: 'human', id: 'usr_other', displayName: 'ana@example.com' } },
    { id: 'e4', action: 'member.removed', subject: 'usr_x', occurredAt: '2026-10-25T09:00:00Z', actor: { kind: 'agent', id: 'inst_1', displayName: 'Helper', harness: 'claude-code' } },
    { id: 'e5', action: 'conversation.shared', subject: 'conv_1', occurredAt: '2026-10-25T08:00:00Z', actor: { kind: 'human', id: 'usr_other', displayName: 'ana@example.com' } },
  ];
  const items = securityNotifications(events, 'usr_me');
  assert.deepEqual(items.map(item => item.id), ['security:e1', 'security:e4']);
  assert.equal(items[0].title, 'ana@example.com connected inst_1');
  assert.equal(items[0].kind, 'security');
  assert.equal(items[0].href, '/app/settings/audit');
  assert.equal(items[1].detail, 'Performed by a connected agent.');
  assert.deepEqual(securityNotifications(events, 'usr_me').some(item => item.id === 'security:e3'), false);
});

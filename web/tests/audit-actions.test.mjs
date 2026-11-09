import test from 'node:test';
import assert from 'node:assert/strict';
import {auditActionPhrase, securityNotificationActions} from '../src/lib/audit.ts';

test('audit action strings map to readable phrases', () => {
 assert.equal(auditActionPhrase('agent.authorized'), 'connected agent');
 assert.equal(auditActionPhrase('member.role-changed'), 'changed a role for');
 assert.equal(auditActionPhrase('conversation.shared'), 'shared conversation');
 assert.equal(auditActionPhrase('memory.deleted'), 'deleted organization note');
 assert.equal(auditActionPhrase('invite.revoked'), 'revoked invitation');
 assert.equal(auditActionPhrase('execution.queued'), 'queued execution');
});
test('unknown actions degrade to words instead of dots', () => {
 assert.equal(auditActionPhrase('something.else-happened'), 'something else happened');
 assert.equal(auditActionPhrase('agent.paused'), 'paused agent');
 assert.equal(securityNotificationActions.has('agent.authorized'), true);
 assert.equal(securityNotificationActions.has('conversation.shared'), false);
});

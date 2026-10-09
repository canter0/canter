// Local UI verification only. Never use this server as a real control plane.
// Visit http://canter-ux.localhost:3007/api/canter/auth/fixture to isolate test cookies.
// Pair with: CANTER_API_ORIGIN=http://127.0.0.1:8087 CANTER_NEXT_DIST_DIR=.next-ux pnpm exec next dev --webpack --hostname 127.0.0.1 --port 3007
import http from 'node:http';

const date = '2026-09-23T16:00:00Z';
const authority = { inspect: true, draft: true, applyMode: 'human' };
const workspace = { id: 'ws_ux_fixture', name: 'Craft workspace', role: 'owner', agentAuthority: authority };
const account = { id: 'acct_ux_fixture', email: 'designer@example.test' };
let state = { offline: false, failSend: false, failDetail: false, paginateHistory: false };
let messagesSent = 0;
const conversations = ['Plan a small VPS for my project', 'Review the staging deployment', 'Connect the documentation repository', 'Check infrastructure spending', 'A very long conversation title that needs to stay readable without pushing the sidebar controls away', 'Review the latest changes'].map((title, i) => ({ id: `conv_fixture_${i}`, workspaceId: workspace.id, title, updatedAt: date, status: i === 1 ? 'failed' : 'completed' }));
const messages = new Map(conversations.map(conversation => [conversation.id, [
  { id: `${conversation.id}_user`, runId: 'run_fixture', role: 'user', content: 'Help me plan a small VPS. I need a clear estimate before taking the next step.', createdAt: date },
  { id: `${conversation.id}_answer`, runId: 'run_fixture', role: 'assistant', content: 'A useful starting point is **2 vCPU and 4 GiB RAM**.\n\nThe Canter compute estimate is **$12 USD per 720 hours**, assuming one server. Disk, operating system, and provider availability still need to be established.\n\n### What will this server run?\n\nThat determines whether this starter configuration is appropriate. This is a plan; no infrastructure has been provisioned.', createdAt: date }
]]));
const systems = [{ revision: 3, contract: { metadata: { name: 'documentation-preview' }, spec: { intent: 'Documentation preview for the team, with a longer description to check wrapping and hierarchy.', services: [{ name: 'web' }] } } }];
const changes = [{ id: 'change_fixture', system: 'documentation-preview', summary: 'Scale the preview app from one to two replicas', phase: 'drafted', createdAt: date }];
const change = { ...changes[0], digest: 'sha256:78f95885a486808d74a719cf96674e151715326f4fb8e5c97d4b10c2189bfcace3', draftedBy: { displayName: 'Local coding agent' }, plan: { baseVersion: 'release-3', impact: { availability: 'Rolling update', data: 'No data migration', monthlyCostDeltaCents: 300 } }, operations: [{ id: 'scale_1', description: 'Increase the web service to two replicas', kind: 'scale', phase: 'queued' }], evidence: [] };
const installations = [{ id: 'agent_fixture', workspaceId: workspace.id, name: 'Local coding agent', harness: 'codex', authority, useWorkspaceAuthority: true, createdAt: date, lastSeenAt: date, workers: [] }];
const initialDeployment = { id: 'deployment_fixture', summary: 'Deploy the documentation preview', phase: 'drafted', digest: change.digest, draftedBy: { displayName: 'Local coding agent' }, plan: { system: systems[0].contract, artifactSha256: change.digest, workspaceRevision: 3, release: { command: ['serve', '/app/public'], publicPort: 8080, healthPath: '/' }, verification: { method: 'GET', path: '/', expectedStatus: 200 } }, operations: [{ id: 'deploy_1', description: 'Start the documentation service', kind: 'deploy', phase: 'queued' }], evidence: [] };
const billing = { usage: { eventCount: 0, resources: [], capacity: [], days: [], start: date, end: date, asOf: date, recordedCents: 0, lastRecordedAt: null, forecast: null, trendReady: false, recentCents: 0, previousCents: 0 }, planId: 'payg', status: 'not_started', paymentReady: false, paymentMethod: null, checkoutEnabled: false, hasBillingAccount: false, periodStart: null, periodEnd: null, cancelAtPeriodEnd: false, bill: { usageCents: 0, totalCents: 0, subscriptionCents: 0, creditAppliedCents: 0, creditRemainingCents: 0, additionalUsageCents: 0 }, pendingEvents: 0, reconciliationEvents: 0 };

const server = http.createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1:8087');
  const path = url.pathname;
  response.setHeader('content-type', 'application/json');
  const send = (value, code = 200) => { response.statusCode = code; response.end(JSON.stringify(value)); };
  let raw = '';
  for await (const chunk of request) raw += chunk;
  let body;
  try { body = raw ? JSON.parse(raw) : {}; } catch { return send({ error: 'Invalid test body' }, 400); }
  if (path === '/test-state') { if (request.method === 'POST') state = { ...state, ...body }; return send({ ...state, messagesSent }); }
  if (path === '/v1/auth/fixture') {
    response.setHeader('set-cookie', 'canter_session=local-ux-fixture; Path=/; HttpOnly; SameSite=Lax');
    response.writeHead(302, { location: '/app' }); return response.end();
  }
  if (path === '/v1/auth/providers') return send({ providers: [{ id: 'github', enabled: false, connected: false }, { id: 'google', enabled: false, connected: false }] });
  if (state.offline) return send({ error: 'Fixture connection is paused.' }, 503);
  if (path === '/v1/me') return send({ account, workspaces: [workspace] });
  if (path === '/v1/installations') return send({ installations });
  if (path.endsWith('/agent-settings') && request.method === 'PATCH') { workspace.agentAuthority = body.authority; return send(workspace); }
  if (path.includes('/systems/') && path.includes('/changes/')) return send(change);
  if (path.endsWith('/systems')) return send({ systems });
  if (path.endsWith('/systems/documentation-preview')) return send({ ...systems[0], bindings: [], issues: [], applicationCapacity: { mode: 'fixed', service: 'web', declaredBaseline: 1, desiredReplicas: 1, readyReplicas: 1, maximumReplicas: 2 } });
  if (path.endsWith('/policies')) return send({ policies: [] });
  if (path.endsWith('/initial-deployments/deployment_fixture')) return send(initialDeployment);
  if (path.endsWith('/changes')) return send({ changes });
  if (path.endsWith('/initial-deployments')) return send({ initialDeployments: [] });
  if (path.endsWith('/tasks')) return send({ tasks: [] });
  if (path.endsWith('/activity')) return send({ actions: [] });
  if (path.endsWith('/billing')) return send(state.breakBilling ? {} : billing);
  if (path.endsWith('/secrets')) return send({ enabled: true, canManage: true, secrets: [{ id: 'secret_fixture', name: 'EXAMPLE_SERVICE_KEY', purpose: 'stored', note: 'Test metadata only. No credential is stored.', version: 2, updatedBy: account.id, updatedAt: date, lastUsedAt: null }] });
  if (path.endsWith('/github')) return send({ enabled: false, connected: false, appEnabled: true });
  if (path.endsWith('/github/repositories')) return send({ connection: { enabled: false, connected: false }, repositories: [], page: 1, hasMore: false });
  if (path.endsWith('/events')) { await new Promise(resolve => setTimeout(resolve, 800)); return send({ events: [] }); }
  if (path.endsWith('/stop')) return send({ status: 'cancelled' });
  const conversationId = path.match(/\/conversations\/([^/]+)/)?.[1];
  if (request.method === 'POST' && (path.endsWith('/conversations') || path.endsWith('/messages'))) {
    if (state.failSend) return send({ error: 'Couldn’t send this message. Your draft is still here.' }, 503);
    messagesSent++;
    const id = conversationId ?? body.id;
    if (!conversations.some(item => item.id === id)) conversations.unshift({ id, workspaceId: workspace.id, title: body.message.slice(0, 65), updatedAt: date, status: 'completed' });
    messages.set(id, [...(messages.get(id) ?? []), { id: `${id}_${messagesSent}`, role: 'user', runId: `run_${messagesSent}`, content: body.message, createdAt: date }, { id: `${id}_${messagesSent}_answer`, role: 'assistant', runId: `run_${messagesSent}`, content: 'The local fixture accepted this message. No real agent was called.', createdAt: date }]);
    return send({ id });
  }
  if (path.endsWith('/conversations')) return send({ conversations, agent: { available: true, model: 'openai/gpt-5.6-luna' } });
  if (conversationId) {
    if (state.failDetail) return send({ error: 'Fixture conversation refresh failed.' }, 503);
    const conversation = conversations.find(item => item.id === conversationId);
    if (request.method === 'PATCH' && conversation) conversation.title = body.title;
    let history = messages.get(conversationId) ?? [];
    if (state.paginateHistory) {
      history = Array.from({ length: 80 }, (_, index) => {
        const turn = Math.floor(index / 2);
        const role = index % 2 ? 'assistant' : 'user';
        return { id: `${conversationId}_history_${index}`, runId: `${conversationId}_history_run_${turn}`, role, content: role === 'user' ? `Earlier fixture question ${turn + 1}` : `Earlier fixture answer ${turn + 1}`, createdAt: new Date(Date.parse(date) + index * 1000).toISOString() };
      });
      const end = Math.min(history.length, Math.max(0, Number(url.searchParams.get('before') ?? history.length) || 0));
      const start = Math.max(0, end - 10);
      return send({ conversation, messages: history.slice(start, end).reverse(), hasMore: start > 0, nextCursor: start > 0 ? String(start) : undefined, run: { id: 'run_fixture', status: 'completed', model: 'fixture' } });
    }
    return send({ conversation, messages: [...history].reverse(), hasMore: false, run: { id: 'run_fixture', status: 'completed', model: 'fixture' } });
  }
  return send({ error: `Fixture endpoint not implemented: ${path}` }, 404);
});
server.listen(8087, '127.0.0.1', () => console.log('UX fixture API: http://127.0.0.1:8087 (synthetic data only)'));

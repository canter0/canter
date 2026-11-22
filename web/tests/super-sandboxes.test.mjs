import test from "node:test";
import assert from "node:assert/strict";
import { superSandboxProgress, superSandboxTerminal, superSandboxStateLabel, superSandboxBlockerLabel, workerDuration, runtimeAllocation, superSandboxRequestPrompt } from "../src/lib/super-sandboxes.ts";
import { dashboardRequestSurface, createDashboardRequest, serializeDashboardRequest, parseDashboardRequest } from "../src/lib/dashboard-requests.ts";
import { wrenSurfacePlacement } from "../src/lib/wren-surface-placement.ts";

const task = (state, index) => ({ id: `task_${index}`, name: `Task ${index}`, index, command: "true", timeoutSeconds: 60, state });

test("unknown outcomes and in-flight cancellation are never counted as settled", () => {
  const states = ["queued", "starting", "running", "unknown", "succeeded", "failed", "cancelled", "outcome_unavailable"];
  assert.deepEqual(superSandboxProgress(states.map(task)), { settled: 4, succeeded: 1, unknown: 1, outcomeUnavailable: 1, total: 8 });
  assert.deepEqual(superSandboxProgress([]), { settled: 0, succeeded: 0, unknown: 0, outcomeUnavailable: 0, total: 0 });
  for (const state of ["queued", "running", "cancelling", "needs_attention", "unknown"]) assert.equal(superSandboxTerminal(state), false);
  for (const state of ["succeeded", "failed", "cancelled"]) assert.equal(superSandboxTerminal(state), true);
  assert.match(superSandboxStateLabel("unknown"), /unknown.*attention/i);
});

test("outcome states and blocker reasons get distinct labels", () => {
  assert.equal(superSandboxStateLabel("outcome_unavailable"), "Outcome unavailable");
  assert.match(superSandboxStateLabel("unknown"), /Outcome unknown/);
  assert.match(superSandboxBlockerLabel("worker_replaced"), /removed or replaced/);
  assert.match(superSandboxBlockerLabel("worker_unavailable"), /sleeping or not ready/);
  assert.match(superSandboxBlockerLabel("workers_busy"), /reserved/);
  assert.match(superSandboxBlockerLabel("worker_removed"), /removed before/);
  assert.match(superSandboxBlockerLabel("start_rejected"), /rejected/);
  assert.equal(superSandboxBlockerLabel(""), "");
  assert.match(superSandboxBlockerLabel("code_from_the_future"), /needs attention/);
});

test("worker runtime allocation preserves reserved upfront time, not elapsed time or cost", () => {
  assert.equal(workerDuration(0), "0s");
  assert.equal(workerDuration(59), "59s");
  assert.equal(workerDuration(60), "1m");
  assert.equal(workerDuration(3661), "1h 1m 1s");
  assert.equal(runtimeAllocation({ reservedWorkerSeconds: 7200, maxWorkerSeconds: 10800 }), "2h / 3h");
});

test("both conversation surfaces use split placement", () => {
  assert.equal(wrenSurfacePlacement("super-sandboxes"), "split");
  assert.equal(wrenSurfacePlacement("super-sandbox"), "split");
});

test("dashboard handoff preserves exact bounded Super IDs, rejects foreign and malformed IDs", () => {
  assert.deepEqual(dashboardRequestSurface({ kind: "super-sandboxes" }), { kind: "super-sandboxes" });
  for (const id of ["ssb_a", "ssb_" + "a".repeat(156), "ssb_case-Sensitive_1"]) {
    const surface = { kind: "super-sandbox", id };
    assert.deepEqual(dashboardRequestSurface(surface), surface);
    const request = createDashboardRequest({ accountId: "a", workspaceId: "w", conversationId: null, prompt: "Review this batch", surface }, 1000, "nonce");
    assert.ok(request);
    assert.deepEqual(parseDashboardRequest(JSON.parse(serializeDashboardRequest(request)), 1000)?.surface, surface);
  }
  for (const id of ["", "ssb_", "sbx_123", " ssb_123", "ssb_123 ", "ssb_two words", "ssb_\n", "ssb_\x00", "ssb_\x7f", "ssb_\u00a0x", "ssb_case-Sensitive:1", "ssb_é", "ssb_日本", "ssb_a.b", "ssb_a/b", "ssb_a+b", "ssb_a=", "ssb_a?", "ssb_" + "a".repeat(157)]) {
    assert.equal(dashboardRequestSurface({ kind: "super-sandbox", id }), null);
  }
  assert.equal(dashboardRequestSurface({ kind: "sandbox", id: "ssb_x" }), null);
  assert.equal(dashboardRequestSurface({ kind: "super-sandboxes", id: "ssb_x" }), null);
  assert.equal(dashboardRequestSurface({ kind: "wren-job", id: "ssb_x" }), null);
});

test("contextual drafts bind organisation and exact resource without auto-execution", () => {
  const create = superSandboxRequestPrompt("wsp_42", "Research");
  assert.match(create, /Research.*wsp_42/);
  assert.match(create, /existing ready workers/);
  assert.match(create, /concurrency.*runtime budget/);
  assert.match(create, /worst-case sum/);
  const cancel = superSandboxRequestPrompt("wsp_42", "Research", "ssb_exact");
  assert.match(cancel, /ssb_exact.*Research.*wsp_42/);
  assert.match(cancel, /unknown tasks/);
  assert.match(cancel, /not automatic execution/);
});

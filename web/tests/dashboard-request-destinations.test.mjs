import { test } from "node:test";
import assert from "node:assert/strict";

import { dashboardRequestDestinations, flattenDashboardRequestDestinations, newCanterDestinationKey } from "../src/lib/dashboard-request-destinations.ts";
import { conversationHref } from "../src/lib/wren-presentation.ts";
import { composerOriginMatchesViewport, createDashboardRequest, dashboardRequestComposerOrigin, parseDashboardRequest, serializeDashboardRequest } from "../src/lib/dashboard-requests.ts";

const conversation = (id, title = "") => ({ id, workspaceId: "wrk_1", title, status: "active", updatedAt: "2026-01-01T00:00:00Z" });
const wren = (id, conversationId, name = "Reader") => ({ id, conversationId, name, status: "active", shape: "circle", palette: 1 });
const room = (id, conversationId, name = "builds") => ({ id, conversationId, ownerId: "acc_1", kind: "channel", name, status: "active", wrens: [], memberIds: [] });

test("Canter group leads with a fresh conversation and keeps general chats", () => {
  const groups = dashboardRequestDestinations({
    conversations: [conversation("conv_general_1", "General chat"), conversation("conv_wren_x"), conversation("conv_room_y")],
    wrens: [],
    rooms: [],
  }, conversationHref);
  assert.equal(groups.canter[0].key, newCanterDestinationKey);
  assert.equal(groups.canter[0].conversationId, null);
  assert.equal(groups.canter[0].href, "/app?compose=1");
  // Backing conversations never take the generic route even when lists fail.
  assert.deepEqual(groups.canter.map(d => d.conversationId), [null, "conv_general_1"]);
});

test("rooms and wrens claim their backing conversations and keep canonical routes", () => {
  const groups = dashboardRequestDestinations({
    conversations: [conversation("conv_wren_1"), conversation("conv_room_1"), conversation("conv_general_1")],
    wrens: [wren("wren_1", "conv_wren_1")],
    rooms: [room("room_1", "conv_room_1", "#builds")],
  }, conversationHref);
  assert.equal(groups.wrens[0].href, "/app/wrens/wren_1");
  assert.equal(groups.rooms[0].href, "/app/wrens/rooms/room_1");
  assert.equal(groups.rooms[0].label, "# builds");
  assert.equal(groups.wrens[0].wren.name, "Reader");
  assert.deepEqual(groups.canter.map(d => d.conversationId), [null, "conv_general_1"]);
});

test("unavailable destinations are excluded entirely", () => {
  const groups = dashboardRequestDestinations({
    conversations: [],
    wrens: [wren("wren_1", "conv_wren_1"), { ...wren("wren_2", "conv_wren_2"), status: "archived" }, { ...wren("wren_3", "conv_wren_3"), sharedAvailable: false }],
    rooms: [{ ...room("room_1", "conv_room_1"), status: "deleted" }],
  }, conversationHref);
  assert.equal(groups.wrens.length, 1);
  assert.equal(groups.rooms.length, 0);
});

test("duplicate claimed conversations are never offered twice", () => {
  const groups = dashboardRequestDestinations({
    conversations: [conversation("conv_a"), conversation("conv_a"), conversation("conv_room_1")],
    wrens: [],
    rooms: [room("room_1", "conv_room_1")],
  }, conversationHref);
  assert.deepEqual(flattenDashboardRequestDestinations(groups).map(d => d.key), ["canter:new", "canter:conv_a", "room:room_1"]);
});

const validOrigin = { x: 100, y: 200, width: 620, height: 180, viewportWidth: 1440, viewportHeight: 900 };

test("composer origin survives a create/serialize/parse roundtrip", () => {
  const request = createDashboardRequest({ accountId: "acc", workspaceId: "wrk", conversationId: null, prompt: "Inspect sandboxes", composerOrigin: validOrigin }, 1000, "nonce_1");
  assert.ok(request);
  assert.deepEqual(request.composerOrigin, validOrigin);
  const reparsed = parseDashboardRequest(JSON.parse(serializeDashboardRequest(request)), 1000);
  assert.deepEqual(reparsed.composerOrigin, validOrigin);
});

test("invalid composer origin is dropped, never a reason to reject", () => {
  for (const bad of [
    { ...validOrigin, x: Number.NaN },
    { ...validOrigin, x: -1 },
    { ...validOrigin, width: 0 },
    { ...validOrigin, viewportWidth: 20000 },
    { ...validOrigin, x: 900, width: 700 },       // x+width beyond viewport+1
    { ...validOrigin, y: 850, height: 100 },      // y+height beyond viewport+1
    "not-an-object",
    { x: 1, y: 2 },
  ]) {
    assert.equal(dashboardRequestComposerOrigin(bad), null);
    const request = createDashboardRequest({ accountId: "acc", workspaceId: "wrk", conversationId: null, prompt: "Inspect sandboxes", composerOrigin: bad }, 1000, "nonce_1");
    assert.ok(request);
    assert.equal(request.composerOrigin, undefined);
    const reparsed = parseDashboardRequest({ nonce: "n", accountId: "acc", workspaceId: "wrk", conversationId: null, prompt: "Inspect", createdAt: 500, composerOrigin: bad }, 1000);
    assert.ok(reparsed);
    assert.equal(reparsed.composerOrigin, undefined);
  }
});

test("viewport match tolerance is one pixel", () => {
  assert.equal(composerOriginMatchesViewport(validOrigin, 1440, 900), true);
  assert.equal(composerOriginMatchesViewport(validOrigin, 1441, 899), true);
  assert.equal(composerOriginMatchesViewport(validOrigin, 1442, 900), false);
  assert.equal(composerOriginMatchesViewport(validOrigin, 390, 844), false);
});

test("envelopes without an origin behave exactly as before", () => {
  const request = createDashboardRequest({ accountId: "acc", workspaceId: "wrk", conversationId: null, prompt: "Inspect sandboxes" }, 1000, "nonce_1");
  assert.equal(request.composerOrigin, undefined);
  const reparsed = parseDashboardRequest(JSON.parse(serializeDashboardRequest(request)), 1000);
  assert.equal(reparsed.composerOrigin, undefined);
});

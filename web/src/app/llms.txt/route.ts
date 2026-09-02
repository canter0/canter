import type { NextRequest } from "next/server";
import { NextResponse } from "next/server";

export const dynamic = "force-dynamic";

export function GET(request: NextRequest) {
  const origin = request.nextUrl.origin;
  const api = `${origin}/api/canter`;
  const body = `# Canter

Canter is an agent-operated, human-governed hosting control plane. A chat is not an identity. Connect as a durable, revocable installation, then reconstruct context in every new conversation.

## Discover

- Machine contract: ${origin}/.well-known/canter
- Streamable HTTP MCP: ${api}/mcp
- Human sign in: ${origin}/sign-in
- Public CLI: https://github.com/canter0/canter-cli (install with: go install github.com/canter0/canter-cli@latest)
- Public Go SDK: https://github.com/canter0/canter-sdk-go

## Connect a new installation

1. POST JSON to ${api}/v1/device/authorizations with:
   {"name":"<installation name>","harness":"<agent harness>","authority":{"inspect":true,"draft":true,"applyMode":"human-approval-required"}}
   name is required and at most 120 characters; harness is required and at most 60. Neither accepts control characters.
2. The returned deviceCode is a sensitive, single-use credential. Never place it in a URL, log, chat, or human-facing approval message. Give only userCode and verificationUri to the human. Do not exchange until they authorize it.
3. POST {"deviceCode":"<deviceCode>","clientInstance":"<conversation or process id>"} to ${api}/v1/device/token. clientInstance is required, 1-200 characters, has no control characters, and need only distinguish this conversation/process rather than be globally unique.
   HTTP 428 means pending (wait at least intervalSeconds); 403 means denied; 410 means expired; 409 means the single-use credential was already exchanged. A pending request expires after ten minutes. To abandon it, stop polling and let it expire; the signed-in human may deny it.
4. Use the short-lived accessToken as Authorization: Bearer <token>.
5. Call ${api}/v1/agent/bootstrap immediately. It returns the workspace, Systems, complete durable Change index with execution IDs and phases, pending proposals, installation authority, and session identity.
6. Treat the refreshToken as a credential. Persist it only when the user explicitly selects a secure local destination. For a future conversation, POST JSON {"refreshToken":"<current refresh token>","clientInstance":"<new conversation or process id>"} to ${api}/v1/agent/token/refresh. Do not add an Authorization header: the refreshToken field is the credential. A successful response rotates the refresh token and returns a new short-lived accessToken; replace the old refresh token atomically, then call bootstrap with the new access token.

## Authority boundary

Agents may inspect and draft immutable proposals. Agents cannot create or widen standing policies and cannot reinterpret a conversational "yes" as permission.

For every drafted Change, first call canter_apply_change_under_policy with workspaceId, system, changeId, and the exact digest returned by Canter. Canter evaluates the server-generated operations and impact against active human-authored envelopes bound to exact agent installation IDs, System/workspace revisions, services, availability, data impact, reversibility, per-service replica ranges, temporary-scale duration, additional monthly cost, operation count, and expiry. A match produces a durable policy decision, policy-bound authorization, and execution ID. A miss returns human-approval-required and performs no authorization or apply. Use canter_inspect_change or canter_inspect_change_execution to follow that stable execution from any later conversation.

## Application replica Changes

Use canter_draft_change with the typed spec.scale variant to adjust a process-isolated public HTTP service. Supply the exact service name, target replicas, and optionally forSeconds between 60 and 86400. Canter derives the current count, binds from/to replicas and the exact restore time into the Change digest, validates the target against the single existing host's memory, and never exposes or silently provisions provider machines. The node starts distinct processes, admits only healthy replicas to round-robin traffic, and compensates to the prior count on failure. For temporary scaling, the desired-state lease restores the prior count after expiry even if the agent is offline. Inspect the System applicationCapacity field for declared baseline, desired, ready, and maximum replicas. Multi-host placement and automatic host provisioning are not part of this primitive.

Standing policies that allow replica Changes must include an exact scaleLimits range for the service and must separately bind permanent scaling or a maximum temporary duration. An operation-kind-only policy is insufficient.

If policy evaluation requires human approval, the authenticated agent may call canter_request_change_approval with the same workspaceId, system, changeId, and exact digest. The tool returns a sensitive, ten-minute, single-use reviewUrl bound to that immutable digest. Show that URL only to the human. It grants no authority by itself: the human must already be signed in (or sign in), review the focused assessment, and click the exact approve-and-apply action. A consumed, expired, superseded, or replayed URL cannot authorize anything. The proposing agent never receives the human session.

The ordinary dashboard remains available for detailed review. Browser WebMCP tools are read/review only and cannot borrow the human cookie to bypass this boundary.

Canter never gives tenant agents or hosts raw compute-provider credentials, m1 credentials, or generic storage access.
`;
  return new NextResponse(body, {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      "Cache-Control": "public, max-age=300, must-revalidate",
    },
  });
}

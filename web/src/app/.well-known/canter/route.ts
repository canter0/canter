import type { NextRequest } from "next/server";
import { NextResponse } from "next/server";
import { publicOrigin } from "@/lib/public-origin";

export const dynamic = "force-dynamic";

export function GET(request: NextRequest) {
  const origin = publicOrigin(request);
  const api = `${origin}/api/canter`;
  return NextResponse.json({
    schemaVersion: "canter.discovery/v1alpha1",
    product: "Canter",
    description: "Agent-operated, human-governed hosting. Conversations are ephemeral clients of durable, revocable agent installations.",
    api,
    mcp: {
      transport: "streamable-http",
      url: `${api}/mcp`,
      authentication: "Bearer access token returned after human device authorization",
    },
    connect: {
      begin: {
        method: "POST",
        url: `${api}/v1/device/authorizations`,
        contentType: "application/json",
        body: {
          name: "A durable installation name chosen by the agent or user",
          harness: "The agent harness, for example codex or claude-code",
          authority: { inspect: true, draft: true, applyMode: "human-approval-required" },
        },
		response: {
		  deviceCode: { type: "string", sensitive: true, description: "Single-use exchange credential. Never put it in a URL, log, chat, or human-facing approval message." },
		  userCode: { type: "string", sensitive: false, description: "Short code intended to be shown to the human." },
		  verificationUri: { type: "string", format: "uri" },
		  expiresAt: { type: "string", format: "date-time" },
		  intervalSeconds: { type: "integer", description: "Minimum polling interval." },
		},
      },
      humanReview: `${origin}/onboarding/authorize?code={userCode}`,
      exchange: {
        method: "POST",
        url: `${api}/v1/device/token`,
        contentType: "application/json",
        body: { deviceCode: "{deviceCode from begin}", clientInstance: "A unique identifier for this conversation or process" },
		constraints: {
		  deviceCode: "Sensitive and single-use.",
		  clientInstance: "Required, 1-200 characters, no control characters. Unique enough to distinguish this conversation or process; global uniqueness is not required.",
		},
		statuses: {
		  "200": "Authorized and exchanged. Returns accessToken, refreshToken, installation, and session.",
		  "428": "Human authorization is still pending. Wait at least intervalSeconds before retrying.",
		  "403": "The human denied the request.",
		  "410": "The device authorization expired.",
		  "409": "The single-use device credential was already exchanged.",
		},
      },
		lifecycle: "The request expires after ten minutes. There is no unauthenticated cancellation endpoint; stop polling and let it expire, or the signed-in human can deny it. Store the refresh token only when the user explicitly chooses a secure local destination. Refresh creates a new short-lived session; bootstrap reconstructs all durable context.",
    },
    authenticated: {
      bootstrap: `${api}/v1/agent/bootstrap`,
      refresh: {
        method: "POST",
        url: `${api}/v1/agent/token/refresh`,
        contentType: "application/json",
        authentication: "No access bearer is required; the refreshToken in the JSON body is the credential.",
        body: {
          refreshToken: "{current refreshToken}",
          clientInstance: "A new identifier for the future conversation or process",
        },
        response: "Returns a rotated refreshToken, a new short-lived accessToken, installation, and session. Replace the old refresh token atomically.",
      },
      identity: `${api}/v1/agent/whoami`,
    },
    governance: {
      agent: ["inspect durable Systems, Changes, executions, and policies", "draft immutable proposals", "request evaluation of one exact Change under existing policy", "request a short-lived human review URL for one exact Change digest"],
      human: ["authenticate", "create or revoke bounded standing policies", "review the bound digest", "consume the single-use route to authorize and enqueue that exact Change"],
      standingPolicies: {
        listTool: "canter_list_standing_policies",
        evaluateTool: "canter_apply_change_under_policy",
        executionTool: "canter_inspect_change_execution",
		semantics: "Only a signed-in human can create or revoke an immutable, expiring policy. Evaluation is bound to the exact server-generated Change digest, policy digest, System revision, allowed agent installation, operations, impact, per-service replica ranges, temporary-scale duration, cost ceiling, and expiry. A miss performs no authorization or apply.",
      },
	  replicaScaling: {
		tool: "canter_draft_change",
		request: { apiVersion: "canter.dev/v1alpha1", kind: "Change", spec: { system: "<system>", summary: "<outcome>", scale: { service: "<public application service>", replicas: 3, forSeconds: 1800 }, verification: { method: "GET", path: "/health", expectedStatus: 200 } } },
		semantics: "Canter derives current capacity, rejects targets outside the single existing host's memory, runs distinct ready-only application processes behind its proxy, and restores the prior count after failure or optional lease expiry. It does not silently provision VMs.",
	  },
      focusedApproval: {
        mcpTool: "canter_request_change_approval",
        lifetimeSeconds: 600,
        semantics: "The URL is human-gated and single-use. It does not let the proposing agent approve its own Change.",
      },
      initialDeployment: {
        computeClasses: ["c1", "c2", "c3"],
        defaultComputeClass: "c1",
        agentTools: ["canter_upload_artifact", "canter_draft_initial_deployment", "canter_list_initial_deployments", "canter_inspect_initial_deployment", "canter_inspect_initial_deployment_execution"],
        humanWorkflow: "The review surface records authorization of the exact digest and then starts that unchanged deployment. These are separate ledger events presented as one explicit approve + start action.",
        failureSemantics: "Contract errors are returned with a stable code, retryability, and valid alternatives. A legacy first deployment that failed on an unsupported class remains immutable history, but the agent may draft a corrected proposal for the same System name because Canter proves no runtime mutation occurred. Provider-sensitive execution failures remain redacted for operator inspection.",
      },
      neverExposed: ["compute provider credentials", "m1 storage credentials", "provider resource identifiers"],
    },
    instructions: `${origin}/llms.txt`,
    distributions: {
      cli: {
        repository: "https://github.com/canter0/canter-cli",
        install: "go install github.com/canter0/canter-cli/cmd/canter@latest",
      },
      goSdk: {
        repository: "https://github.com/canter0/canter-sdk-go",
        module: "github.com/canter0/canter-sdk-go",
      },
    },
  }, {
    headers: { "Cache-Control": "public, max-age=300, must-revalidate" },
  });
}

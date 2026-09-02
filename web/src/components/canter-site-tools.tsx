"use client";

import { useEffect } from "react";
import { canterFetch, type ChangeDetail, type InitialDeploymentDetail, type InitialDeploymentSummary, type Installation, type Me, type StandingPolicy, type SystemRecord } from "@/lib/canter-api";

type ToolExecutionOptions = { signal: AbortSignal };
type SiteTool = {
  name: string;
  title?: string;
  description: string;
  inputSchema?: Record<string, unknown>;
  annotations?: { readOnlyHint?: boolean; untrustedContentHint?: boolean };
  execute(input: Record<string, unknown>, options: ToolExecutionOptions): Promise<unknown>;
};

type ModelContext = {
  registerTool(tool: SiteTool, options?: { exposedTo?: string[]; signal?: AbortSignal }): Promise<void>;
};

declare global {
  interface Document {
    modelContext?: ModelContext;
  }
}

const workspaceInput = {
  type: "object",
  properties: {
    workspaceId: { type: "string", description: "Canter workspace ID. Omit to use the first workspace available to the signed-in account." },
  },
  additionalProperties: false,
} as const;

async function resolveWorkspace(input: Record<string, unknown>): Promise<{ me: Me; workspaceId: string }> {
  const me = await canterFetch<Me>("/me");
  const requested = typeof input.workspaceId === "string" ? input.workspaceId : "";
  const workspace = requested ? me.workspaces.find((item) => item.id === requested) : me.workspaces[0];
  if (!workspace) throw new Error("The signed-in account has no accessible Canter workspace.");
  return { me, workspaceId: workspace.id };
}

function tools(): SiteTool[] {
  return [
    {
      name: "canter_bootstrap",
      title: "Inspect Canter workspace",
      description: "Read the signed-in user's Canter identity, workspace, systems, governed Changes, and authorized agent installations. This does not mutate infrastructure.",
      inputSchema: workspaceInput,
      annotations: { readOnlyHint: true, untrustedContentHint: true },
      async execute(input, { signal }) {
        signal.throwIfAborted();
        const { me, workspaceId } = await resolveWorkspace(input);
        const [systems, changes, initialDeployments, installations] = await Promise.all([
          canterFetch<{ systems: SystemRecord[] }>(`/workspaces/${workspaceId}/systems`, { signal }),
          canterFetch<{ changes: ChangeDetail[] }>(`/workspaces/${workspaceId}/changes`, { signal }),
          canterFetch<{ initialDeployments: InitialDeploymentSummary[] }>(`/workspaces/${workspaceId}/initial-deployments`, { signal }),
          canterFetch<{ installations: Installation[] }>(`/installations?workspaceId=${encodeURIComponent(workspaceId)}`, { signal }),
        ]);
        return { account: me.account, workspace: me.workspaces.find((item) => item.id === workspaceId), systems: systems.systems, changes: changes.changes, initialDeployments: initialDeployments.initialDeployments, installations: installations.installations };
      },
    },
    {
      name: "canter_inspect_initial_deployment",
      title: "Inspect a first deployment proposal",
      description: "Read an immutable first-deployment proposal, its exact digest, System contract, artifact digest, operation plan, human authorization, and evidence. This does not upload an artifact or mutate infrastructure.",
      inputSchema: {
        type: "object",
        properties: { workspaceId: { type: "string" }, deploymentId: { type: "string" } },
        required: ["deploymentId"],
        additionalProperties: false,
      },
      annotations: { readOnlyHint: true, untrustedContentHint: true },
      async execute(input, { signal }) {
        signal.throwIfAborted();
        const { workspaceId } = await resolveWorkspace(input);
        if (typeof input.deploymentId !== "string" || !input.deploymentId) throw new Error("deploymentId is required");
        return canterFetch<InitialDeploymentDetail>(`/workspaces/${workspaceId}/initial-deployments/${encodeURIComponent(input.deploymentId)}`, { signal });
      },
    },
    {
      name: "canter_inspect_system",
      title: "Inspect a Canter system",
      description: "Read declared, observed, and public state for one System in the signed-in Canter workspace. This does not mutate infrastructure.",
      inputSchema: {
        type: "object",
        properties: { workspaceId: { type: "string" }, system: { type: "string" } },
        required: ["system"],
        additionalProperties: false,
      },
      annotations: { readOnlyHint: true, untrustedContentHint: true },
      async execute(input, { signal }) {
        signal.throwIfAborted();
        const { workspaceId } = await resolveWorkspace(input);
        if (typeof input.system !== "string" || !input.system) throw new Error("system is required");
        return canterFetch(`/workspaces/${workspaceId}/systems/${encodeURIComponent(input.system)}`, { signal });
      },
    },
    {
      name: "canter_inspect_change",
      title: "Inspect a governed Change",
      description: "Read the exact plan, digest, actor attribution, operation ledger, evidence, and durable execution identity for one Canter Change. This does not mutate infrastructure.",
      inputSchema: {
        type: "object",
        properties: { workspaceId: { type: "string" }, system: { type: "string" }, changeId: { type: "string" } },
        required: ["system", "changeId"],
        additionalProperties: false,
      },
      annotations: { readOnlyHint: true, untrustedContentHint: true },
      async execute(input, { signal }) {
        signal.throwIfAborted();
        const { workspaceId } = await resolveWorkspace(input);
        if (typeof input.system !== "string" || typeof input.changeId !== "string") throw new Error("system and changeId are required");
        return canterFetch(`/workspaces/${workspaceId}/systems/${encodeURIComponent(input.system)}/changes/${encodeURIComponent(input.changeId)}`, { signal });
      },
    },
		{
			name: "canter_list_standing_policies",
			title: "Inspect standing policies",
			description: "Read immutable human-authored policy envelopes, including exact agent installations, impact bounds, revisions, expiry, and revocation. Browser tools cannot create, widen, invoke, or revoke policy authority.",
			inputSchema: {
				type: "object",
				properties: { workspaceId: { type: "string" }, system: { type: "string" } },
				required: ["system"],
				additionalProperties: false,
			},
			annotations: { readOnlyHint: true, untrustedContentHint: true },
			async execute(input, { signal }) {
				signal.throwIfAborted();
				const { workspaceId } = await resolveWorkspace(input);
				if (typeof input.system !== "string" || !input.system) throw new Error("system is required");
				return canterFetch<{ policies: StandingPolicy[] }>(`/workspaces/${workspaceId}/systems/${encodeURIComponent(input.system)}/policies`, { signal });
			},
		},
  ];
}

export function CanterSiteTools() {
  useEffect(() => {
    const context = document.modelContext;
    if (!context || typeof context.registerTool !== "function") return;
    const lifecycle = new AbortController();
    let disposed = false;
    void (async () => {
      for (const tool of tools()) {
        if (disposed) return;
        try {
          await context.registerTool(tool, { signal: lifecycle.signal });
        } catch (error) {
          if (!lifecycle.signal.aborted) console.warn(`Canter could not register ${tool.name}`, error);
        }
      }
    })();
    return () => {
      disposed = true;
      lifecycle.abort();
    };
  }, []);

  return null;
}

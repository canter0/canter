import { canterFetch } from "./canter-api";
export type Conversation = { id: string; workspaceId: string; title: string; updatedAt: string; status: string };
export type OperatorMessage = { id: string; runId: string; role: "user" | "assistant"; content: string; createdAt: string };
export type OperatorRun = { id: string; status: "queued" | "running" | "completed" | "failed" | "cancelled"; model: string; failure?: string };
export type OperatorSurface = { kind: "apps" | "deployments" | "billing" | "activity" | "agents" | "app" | "deployment" | "change" | "repository" | "github"; id?: string; system?: string; repository?: string };
export type OperatorEvent = { sequence: number; runId?: string; kind: string; data: Record<string, unknown>; createdAt: string };
export type ConversationDetail = { conversation: Conversation; messages: OperatorMessage[]; run: OperatorRun | null };
export const conversationBase = (workspace: string) => `/workspaces/${encodeURIComponent(workspace)}/conversations`;
export const conversationDetail = (workspace: string, id: string, signal?: AbortSignal) => canterFetch<ConversationDetail>(`${conversationBase(workspace)}/${encodeURIComponent(id)}`, { signal });
export const surfaceLabels: Record<OperatorSurface["kind"], string> = { apps: "Apps", deployments: "Deployments", billing: "Billing", activity: "Activity", agents: "Agent access", app: "App", deployment: "Deployment review", change: "Change review", repository: "Repository", github: "GitHub" };
export function surfaceKey(surface: OperatorSurface) { return [surface.kind, surface.id, surface.system, surface.repository].filter(Boolean).join(":"); }
export function isSurface(value: Record<string, unknown>): value is Record<string, unknown> & OperatorSurface { return typeof value.kind === "string" && value.kind in surfaceLabels; }

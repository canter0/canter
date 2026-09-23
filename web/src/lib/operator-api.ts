import { canterFetch } from "./canter-api";
export type Conversation = { id: string; workspaceId: string; title: string; updatedAt: string; status: string };
export type OperatorAttachment = { id: string; name: string; mediaType: string; dataBase64: string; size: number };
export type OperatorMessage = { id: string; runId: string; role: "user" | "assistant"; content: string; attachments?: OperatorAttachment[]; surface?: OperatorSurface | null; createdAt: string };
export type OperatorRun = { id: string; status: "queued" | "running" | "completed" | "failed" | "cancelled"; model: string; failure?: string };
export type OperatorSurface = { kind: "compute" | "storage" | "apps" | "deployments" | "billing" | "activity" | "agents" | "app" | "deployment" | "change" | "repository" | "github" | "file" | "repository-changes" | "conversation"; id?: string; system?: string; repository?: string; base?: string; path?: string };
export type OperatorEvent = { sequence: number; runId?: string; kind: string; data: Record<string, unknown>; createdAt: string };
export type ConversationDetail = { conversation: Conversation; messages: OperatorMessage[]; run: OperatorRun | null };
export const conversationBase = (workspace: string) => `/workspaces/${encodeURIComponent(workspace)}/conversations`;
export const conversationDetail = (workspace: string, id: string, signal?: AbortSignal) => canterFetch<ConversationDetail>(`${conversationBase(workspace)}/${encodeURIComponent(id)}`, { signal });
export const surfaceLabels: Record<OperatorSurface["kind"], string> = { compute: "VPS setup", storage: "Storage bucket", apps: "Apps", deployments: "Deployments", billing: "Billing", activity: "Activity", agents: "Agent access", app: "App", deployment: "Deployment review", change: "Change review", repository: "Repository", github: "GitHub", file: "Source", "repository-changes": "Changes", conversation: "Conversation" };
export function surfaceKey(surface: OperatorSurface) { return [surface.kind, surface.id, surface.system, surface.repository, surface.base, surface.path].filter(Boolean).join(":"); }
export function isSurface(value: Record<string, unknown>): value is Record<string, unknown> & OperatorSurface { return typeof value.kind === "string" && value.kind in surfaceLabels; }

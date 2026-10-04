import "server-only";

import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { cache } from "react";
import { CanterAPIError, type Me } from "./canter-api";
import type { ConversationDetail } from "./operator-api";
import type { WorkspaceBootstrap, WorkspaceOverview } from "./workspace-data";

const canterAPIOrigin = process.env.CANTER_API_ORIGIN ?? "http://127.0.0.1:8081";

export async function serverCanterFetch<T>(path: string): Promise<T> {
  const store = await cookies();
  const session = store.get("__Host-canter_session") ?? store.get("canter_session");
  if (!session) throw new CanterAPIError(401, "Sign in to continue.");
  const response = await fetch(`${canterAPIOrigin}/v1${path}`, {
    headers: { cookie: `${session.name}=${session.value}` },
    cache: "no-store",
  });
  if (!response.ok) throw new CanterAPIError(response.status, "Your workspace could not be loaded.");
  return response.json() as Promise<T>;
}

// React cache is request-scoped: layouts and pages share this read, never users.
const authenticated = cache(async (): Promise<Me | null> => {
  try {
    return await serverCanterFetch<Me>("/me");
  } catch {
    return null;
  }
});

export async function redirectAuthenticated(destination = "/app") {
  if (await authenticated()) redirect(destination);
}

export async function requireAuthenticated() {
  const me = await authenticated();
  if (!me) redirect("/sign-in?next=/app");
  return me;
}

export const workspaceBootstrap = cache(async (): Promise<WorkspaceBootstrap> => {
  const me = await requireAuthenticated();
  const workspace = me.workspaces[0];
  if (!workspace) throw new Error("No workspace is available.");
  const data: WorkspaceOverview = { account: me.account, workspace, conversations: [], agent: { available: false, model: "" }, systems: [], installations: [], changes: [], initialDeployments: [], tasks: [], actions: [] };
  try {
    const conversations = await serverCanterFetch<Pick<WorkspaceOverview, "conversations" | "agent">>(`/workspaces/${encodeURIComponent(workspace.id)}/conversations`);
    return { data: { ...data, ...conversations, conversations: conversations.conversations ?? [] }, conversationsLoaded: true };
  } catch {
    // A failed list read must remain retryable on the client, not become an empty list.
    return { data, conversationsLoaded: false };
  }
});

export async function initialConversation(id: string): Promise<{ detail: ConversationDetail; loadedAt: number } | null> {
  const me = await requireAuthenticated();
  const workspace = me.workspaces[0];
  if (!workspace) return null;
  try {
    const detail = await serverCanterFetch<ConversationDetail>(`/workspaces/${encodeURIComponent(workspace.id)}/conversations/${encodeURIComponent(id)}`);
    return { detail, loadedAt: Date.now() };
  } catch {
    // The client retains its reconnect/error UI if the initial read is unavailable.
    return null;
  }
}

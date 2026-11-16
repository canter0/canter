import { WrenConversation } from "@/components/wrens-workspace";
import { initialConversation, serverCanterFetch, selectedWorkspace, requireAuthenticated } from "@/lib/server-auth";
import type { Wren } from "@/lib/wrens";
import { notFound } from "next/navigation";
import { CanterAPIError } from "@/lib/canter-api";
export default async function WrenPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const me = await requireAuthenticated();
 const workspace=await selectedWorkspace(me);
  let wren: Wren;
  try { wren = await serverCanterFetch<Wren>(`/workspaces/${encodeURIComponent(workspace.id)}/wrens/${encodeURIComponent(id)}`); }
  catch (error) { if (error instanceof CanterAPIError && error.status === 404) notFound(); throw error; }
  const initial = await initialConversation(wren.conversationId);
  return <WrenConversation wren={wren} initialDetail={initial?.detail ?? null} initialLoadedAt={initial?.loadedAt} />;
}

import { OperatorWorkspace } from "@/components/operator-workspace";
import { initialConversation } from "@/lib/server-auth";

export default async function ConversationPage({ params, searchParams }: { params: Promise<{ id: string }>; searchParams: Promise<{ github?: string }> }) {
  const { id } = await params;
  const [{ github }, detail] = await Promise.all([searchParams, initialConversation(id)]);
  return <OperatorWorkspace key={`${id}:${github ?? ""}`} id={id} githubResult={github} initialDetail={detail?.detail ?? null} initialLoadedAt={detail?.loadedAt} />;
}

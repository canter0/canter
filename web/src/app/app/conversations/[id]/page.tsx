import { OperatorWorkspace } from "@/components/operator-workspace";

export default async function ConversationPage({ params, searchParams }: { params: Promise<{ id: string }>; searchParams: Promise<{ github?: string }> }) {
  const { id } = await params;
  const { github } = await searchParams;
  return <OperatorWorkspace key={`${id}:${github ?? ""}`} id={id} githubResult={github} />;
}

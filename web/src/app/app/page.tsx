import { OperatorWorkspace } from "@/components/operator-workspace";

export default async function Dashboard({ searchParams }: { searchParams: Promise<{ github?: string; compose?: string; welcome?: string }> }) {
  const { github, compose, welcome } = await searchParams;
  return <OperatorWorkspace key={welcome === "1" ? `welcome:${github ?? ""}` : "workspace"} githubResult={github} focusComposer={compose === "1"} welcome={welcome === "1"} />;
}

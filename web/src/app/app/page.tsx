import { OperatorWorkspace } from "@/components/operator-workspace";

export default async function Dashboard({ searchParams }: { searchParams: Promise<{ github?: string; compose?: string }> }) {
  const { github, compose } = await searchParams;
  return <OperatorWorkspace githubResult={github} focusComposer={compose === "1"} />;
}

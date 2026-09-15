import { OperatorWorkspace } from "@/components/operator-workspace";

export default async function Dashboard({ searchParams }: { searchParams: Promise<{ github?: string }> }) {
  const { github } = await searchParams;
  return <OperatorWorkspace githubResult={github} />;
}

import { OperatorWorkspace } from "@/components/operator-workspace";
import { requireAuthenticated } from "@/lib/server-auth";
import { showOnboarding } from "@/lib/onboarding";

export default async function Dashboard({ searchParams }: { searchParams: Promise<{ github?: string; compose?: string; welcome?: string }> }) {
  const { github, compose, welcome } = await searchParams;
  const me = await requireAuthenticated();
  const onboarding = showOnboarding(me.onboardingComplete, welcome, compose);
  return <OperatorWorkspace key={onboarding ? `welcome:${github ?? ""}` : "workspace"} githubResult={github} focusComposer={compose === "1"} welcome={onboarding} />;
}

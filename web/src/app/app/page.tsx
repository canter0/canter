import { OperatorWorkspace } from "@/components/operator-workspace";
import { requireAuthenticated } from "@/lib/server-auth";
import { showOnboarding } from "@/lib/onboarding";

export default async function Dashboard({ searchParams }: { searchParams: Promise<{ github?: string; compose?: string; welcome?: string; prompt?: string; draftTab?: string }> }) {
  const { github, compose, welcome, prompt, draftTab } = await searchParams;
  const me = await requireAuthenticated();
  const onboarding = showOnboarding(me.onboardingComplete, welcome, compose);
  // draftTab marks a workspace-tab blank draft: validated ids remount the
  // workspace so independent Home tabs never share a draft or local state.
  const validDraftTab = draftTab && /^[a-z0-9-]{1,80}$/i.test(draftTab) ? draftTab : undefined;
  return <OperatorWorkspace key={onboarding ? `welcome:${github ?? ""}` : validDraftTab ? `tab:${validDraftTab}` : "workspace"} githubResult={github} focusComposer={compose === "1"} welcome={onboarding} prefillPrompt={prompt} draftTab={validDraftTab} />;
}

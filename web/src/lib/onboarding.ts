export function showOnboarding(complete: boolean | undefined, welcome?: string, compose?: string) {
  return welcome === "1" || (complete === false && compose !== "1");
}

type OnboardingWrite = (path: string, init: { method: "POST"; keepalive: true }) => Promise<unknown>;
export async function completeOnboarding(request: OnboardingWrite): Promise<void> {
  await request("/me/onboarding", { method: "POST", keepalive: true });
}
export async function openWrenOnboarding(request: OnboardingWrite, navigate: (destination: string) => void): Promise<void> {
  await completeOnboarding(request);
  navigate("/app/wrens/new");
}

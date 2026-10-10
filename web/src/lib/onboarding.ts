export function showOnboarding(complete: boolean | undefined, welcome?: string, compose?: string) {
  return welcome === "1" || (complete === false && compose !== "1");
}

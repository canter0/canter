import catalog from "../../../internal/controlplane/operator_models.json";

export type OperatorModelOptions = { reasoningEffort?: string };
export type OperatorModelChoice = { id: string; name: string; provider: string; logo: string; contextTokens?: number; reasoningEfforts?: readonly string[]; defaultReasoningEffort?: string };
export const operatorModels: readonly OperatorModelChoice[] = catalog;
export const reasoningLabels: Record<string, string> = { none: "Off", on: "On", low: "Low", medium: "Medium", high: "High", xhigh: "Extra high", max: "Max" };

export function operatorModelChoice(id?: string): OperatorModelChoice | undefined {
  if (!id) return;
  const known = operatorModels.find(model => model.id === id);
  if (known) return known;
  if (id === "openai/gpt-5.6-luna") return { ...operatorModels[0], id, name: "GPT-5.6 Luna" };
  return { id, name: id.split("/").at(-1) ?? id, provider: "Workspace model", logo: "" };
}

export function operatorModelOptions(id?: string, saved?: OperatorModelOptions): OperatorModelOptions {
  const choice = operatorModelChoice(id);
  if (!choice?.reasoningEfforts) return {};
  return { reasoningEffort: choice.reasoningEfforts.includes(saved?.reasoningEffort ?? "") ? saved!.reasoningEffort : choice.defaultReasoningEffort };
}

export function parseModelPreferences(raw: string): Record<string, OperatorModelOptions> {
  try {
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    return Object.fromEntries(Object.entries(parsed).filter(([, value]) => value && typeof value === "object").map(([id, value]) => [id, operatorModelOptions(id, value as OperatorModelOptions)]));
  } catch { return {}; }
}

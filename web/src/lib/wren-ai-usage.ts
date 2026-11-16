export type WrenAIUsageSnapshot = {
  configured: boolean; enforced: boolean; currency: string; unit: string;
  limitMicroUsd: number; heldMicroUsd: number; spentMicroUsd: number; remainingMicroUsd: number;
  estimatedHoldsMicroUsd: number; reservedEstimateMicroUsd: number; dispatchedEstimateMicroUsd: number; uncertainEstimateMicroUsd: number;
  closed: boolean; computeIncluded: boolean;
};
export type AIUsageMode = "hidden" | "unavailable" | "tracking" | "paused" | "active";
const validAmount = (amount: unknown): amount is number => typeof amount === "number" && Number.isSafeInteger(amount) && amount >= 0;
export function formatAIMicroUSD(amount: number): string {
  if (!validAmount(amount)) return "Unavailable";
  return new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 6 }).format(amount / 1_000_000);
}
export function aiUsageMode(value: WrenAIUsageSnapshot): AIUsageMode {
  if (!value || typeof value.configured !== "boolean" || typeof value.enforced !== "boolean") return "unavailable";
  if (!value.configured && !value.enforced) return "hidden";
  if (!value.configured || value.currency !== "USD" || value.unit !== "micro_usd" || value.computeIncluded !== false || typeof value.closed !== "boolean") return "unavailable";
  if (![value.limitMicroUsd,value.heldMicroUsd,value.spentMicroUsd,value.remainingMicroUsd,value.estimatedHoldsMicroUsd,value.reservedEstimateMicroUsd,value.dispatchedEstimateMicroUsd,value.uncertainEstimateMicroUsd].every(validAmount)) return "unavailable";
  if (!value.enforced) return "tracking";
  return value.closed || value.remainingMicroUsd === 0 ? "paused" : "active";
}

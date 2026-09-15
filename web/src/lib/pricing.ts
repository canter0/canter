import catalog from "../../../pricing/catalog.json";

export type PlanID = "payg" | "pro";
export const pricingCatalog = catalog;
export function estimateBill(planID: PlanID, usageCents: number) {
  if (!Number.isSafeInteger(usageCents) || usageCents < 0 || usageCents > 1_000_000_000) throw new Error("Invalid usage amount");
  const plan = catalog.plans.find((item) => item.id === planID);
  if (!plan) throw new Error("Unknown plan");
  const creditAppliedCents = Math.min(usageCents, plan.includedUsageCents);
  return { subscriptionCents: plan.monthlyCents, usageCents, creditAppliedCents, additionalUsageCents: usageCents - creditAppliedCents, totalCents: plan.monthlyCents + usageCents - creditAppliedCents };
}
export function dollars(cents: number) {
  return new Intl.NumberFormat("en-US", { style: "currency", currency: catalog.currency, minimumFractionDigits: cents % 100 ? 2 : 0, maximumFractionDigits: 2 }).format(cents / 100);
}
export function planStartURL(plan: PlanID) {
  return "/create-account?next=" + encodeURIComponent("/app/billing?plan=" + plan);
}

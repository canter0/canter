import type { Metadata } from "next";
import { BillingSettings } from "@/components/billing-settings";

export const metadata: Metadata = { title: "Billing" };

export default async function BillingPage({ searchParams }: { searchParams: Promise<{ plan?: string; checkout?: string }> }) {
  const params = await searchParams;
  return <BillingSettings initialPlan={params.plan === "payg" ? "payg" : "pro"} checkoutReturned={params.checkout === "complete"} />;
}

import type { Metadata } from "next";
import { BillingSettings } from "@/components/billing-settings";

export const metadata: Metadata = { title: "Billing" };

export default async function BillingPage({ searchParams }: { searchParams: Promise<{ plan?: string; checkout?: string; view?: string }> }) {
  const params = await searchParams;
  return <BillingSettings initialPlan={params.plan === "pro" ? "pro" : "payg"} checkoutReturned={params.checkout === "complete"} section={params.view === "plans" || params.view === "invoices" ? params.view : "usage"} showPlan={params.plan === "payg" || params.plan === "pro"} />;
}

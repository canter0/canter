"use client";

import Link from "next/link";
import { BillingUsageView, type BillingUsage } from "./billing-usage";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import { useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { useWorkspace } from "@/components/workspace-context";
import { canterFetch } from "@/lib/canter-api";
import { dollars, estimateBill, type PlanID } from "@/lib/pricing";
import styles from "@/app/app/billing/billing.module.css";

type BillingState = {
 usage: BillingUsage;
  planId: PlanID; status: string; checkoutEnabled: boolean; hasBillingAccount: boolean; periodStart: string | null; periodEnd: string | null; cancelAtPeriodEnd: boolean;
  bill: ReturnType<typeof estimateBill> & { creditRemainingCents: number }; pendingEvents: number; reconciliationEvents: number;
};

export function BillingSettings({ initialPlan, checkoutReturned }: { initialPlan: PlanID; checkoutReturned: boolean }) {
  const { data } = useWorkspace();
  const scopedWorkspace = useSurfaceWorkspace();
  const workspaceID = scopedWorkspace ?? data?.workspace.id;
  const [state, setState] = useState<BillingState | null>(null);
  const [selected, setSelected] = useState<PlanID>(initialPlan);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const awaitingPayment = checkoutReturned;
  const [reload, setReload] = useState(0);

  useEffect(() => {
    if (!workspaceID) return;
    let cancelled = false;
    void canterFetch<BillingState>("/workspaces/" + encodeURIComponent(workspaceID) + "/billing")
      .then((result) => { if (!cancelled) { setState(result); setError(""); } })
      .catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : "Billing could not be loaded."); });
    return () => { cancelled = true; };
  }, [workspaceID, reload]);
  useEffect(() => {
    if (!awaitingPayment || state?.status === "active") return;
    let ticks = 0;
    const timer = setInterval(() => { setReload((value) => value + 1); if (++ticks >= 12) clearInterval(timer); }, 5000);
    return () => clearInterval(timer);
  }, [awaitingPayment, state?.status]);

  async function openPayment(action: "checkout" | "portal") {
    if (!workspaceID) return;
    setBusy(true); setError("");
    try {
      const result = await canterFetch<{ url: string }>("/workspaces/" + encodeURIComponent(workspaceID) + "/billing/" + action, { method: "POST", body: JSON.stringify(action === "checkout" ? { planId: selected } : {}) });
      const target = new URL(result.url);
      if (target.protocol !== "https:" || !["checkout.stripe.com", "billing.stripe.com"].includes(target.hostname)) throw new Error("The payment provider returned an invalid link.");
      window.location.assign(target.href);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "The payment provider could not be reached."); setBusy(false); }
  }
  const started = !!state && !["not_started", "canceled", "incomplete_expired"].includes(state.status);
  return <AppShell active="Billing"><div className={styles.page}>
    <div className={styles.heading}><div><h1>Usage & spending</h1><p>Where your resources go, and what comes next.</p></div><Link href="/pricing">View pricing ↗</Link></div>
    {error ? <div className={styles.notice} role="alert">{error} <button onClick={() => setReload((value) => value + 1)}>Retry</button></div> : null}
    {!state && !error ? <p role="status">Loading billing…</p> : null}
    {state?.usage ? <BillingUsageView usage={state.usage} plan={state.planId} /> : null}
    <details className={styles.paymentSettings}><summary>Plan & payment<span>{state?.planId === "pro" ? "Pro" : "Pay as you go"}</span></summary>
    {state && !state.checkoutEnabled ? <div className={styles.notice} role="status">Payments are not open yet. You can review both plans below; no subscription will start and no payment will be taken.</div> : null}
    {awaitingPayment && state?.status !== "active" ? <div className={styles.notice} role="status">Waiting for payment confirmation. Your plan changes only after the payment provider confirms it. <button onClick={() => setReload((value) => value + 1)}>Refresh</button></div> : null}
    {state && started ? <>
      <section className={styles.current}><div><span className="meta">Current plan</span><h2>{state.planId === "pro" ? "Pro" : "Pay as you go"}</h2><p>{state.status === "active" ? "Active" : state.status.replaceAll("_", " ")}{state.cancelAtPeriodEnd ? " · Ends at the close of this billing period" : ""}</p></div><button disabled={busy || !state.checkoutEnabled} onClick={() => openPayment("portal")}>{busy ? "Opening…" : "Manage billing"}</button></section>
      <section className={styles.statement} aria-label="Current billing period"><header><h2>This billing period</h2><p>{state.periodStart ? new Date(state.periodStart).toLocaleDateString() : "—"} – {state.periodEnd ? new Date(state.periodEnd).toLocaleDateString() : "—"}</p></header>
        <dl><div><dt>Subscription</dt><dd>{dollars(state.bill.subscriptionCents)}</dd></div><div><dt>Recorded usage</dt><dd>{dollars(state.bill.usageCents)}</dd></div><div><dt>Usage credit applied</dt><dd>−{dollars(state.bill.creditAppliedCents)}</dd></div><div><dt>Additional usage</dt><dd>{dollars(state.bill.additionalUsageCents)}</dd></div><div className={styles.total}><dt>Estimated period total</dt><dd>{dollars(state.bill.totalCents)}</dd></div></dl>
        <p className={styles.small}>Before tax. This period’s total includes the subscription paid at the start. Usage may take time to arrive; finalized invoices are available under Manage billing.</p>
        {state.pendingEvents > 0 ? <p className={styles.small}>Some recorded usage is still syncing to your invoice.</p> : null}
        {state.reconciliationEvents > 0 ? <p className={styles.small}>Some usage needs reconciliation before the invoice can be confirmed.</p> : null}
      </section>
    </> : <section className={styles.selection} aria-labelledby="choose-plan"><h2 id="choose-plan">Choose how you pay</h2><div className={styles.plans} role="group" aria-label="Billing plan">{(["payg", "pro"] as const).map((plan) => <button key={plan} aria-pressed={selected === plan} onClick={() => setSelected(plan)}><span>{plan === "pro" ? "Pro" : "Pay as you go"}</span><strong>{plan === "pro" ? "$20" : "$0"}<small>/month</small></strong><p>{plan === "pro" ? "Includes $20 of usage credit. Only pay extra above it." : "No subscription fee. Pay for your resource usage."}</p></button>)}</div>
      <p className={styles.small}>{selected === "pro" ? "$20 is charged when you subscribe and each billing month. $21 of usage means $1 extra, for $21 total. Credit resets each month." : "Add a payment method to enable monthly usage billing. There is no monthly subscription fee."} All prices are USD, before tax.</p>
      <button className={styles.checkout} disabled={busy || !state?.checkoutEnabled} onClick={() => openPayment("checkout")}>{busy ? "Opening secure checkout…" : "Continue to payment"}</button>
    </section>}
    {!started && state?.hasBillingAccount ? <p><button className={styles.checkout} disabled={busy || !state.checkoutEnabled} onClick={() => openPayment("portal")}>Invoices & payment methods</button></p> : null}
    </details>
    <Link className={styles.back} href="/app/account">← Account settings</Link>
  </div></AppShell>;
}

"use client";

import Link from "next/link";
import { BillingUsageView, type BillingUsage } from "./billing-usage";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import { useEffect, useId, useState } from "react";
import { SettingsShell } from "@/components/settings-shell";
import settings from "@/components/settings.module.css";
import { useWorkspace } from "@/components/workspace-context";
import { canterFetch } from "@/lib/canter-api";
import { dollars, estimateBill, type PlanID } from "@/lib/pricing";
import styles from "@/app/app/billing/billing.module.css";

type BillingState = {
 usage: BillingUsage;
  pendingPlanId?: PlanID; pendingPlanAt?: string;
  paymentReady: boolean; paymentMethod: { brand: string; last4: string } | null;
  planId: PlanID; status: string; checkoutEnabled: boolean; hasBillingAccount: boolean; periodStart: string | null; periodEnd: string | null; cancelAtPeriodEnd: boolean;
  bill: ReturnType<typeof estimateBill> & { creditRemainingCents: number }; pendingEvents: number; reconciliationEvents: number;
};

export function BillingSettings({ initialPlan, checkoutReturned, showPlan = false, section = "usage" }: { initialPlan: PlanID; checkoutReturned: boolean; showPlan?: boolean; section?: "usage" | "plans" | "invoices" }) {
  const { data } = useWorkspace();
  const scopedWorkspace = useSurfaceWorkspace();
  const workspaceID = scopedWorkspace ?? data?.workspace.id;
  const [state, setState] = useState<BillingState | null>(null);
  const [selected, setSelected] = useState<PlanID>(initialPlan);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const awaitingPayment = checkoutReturned;
  const [reload, setReload] = useState(0);
  const view = showPlan || checkoutReturned ? "plans" : section;
  const [usageTab, setUsageTab] = useState<"overview" | "history">("overview");
  const canManage = data?.workspace.role === "owner";
  const viewID = useId();

  useEffect(() => {
    if (!workspaceID) return;
    let cancelled = false;
    void canterFetch<BillingState>("/workspaces/" + encodeURIComponent(workspaceID) + "/billing")
      .then((result) => { if (!cancelled) { setState(result); setError(""); } })
      .catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : "Billing could not be loaded."); });
    return () => { cancelled = true; };
  }, [workspaceID, reload]);
  useEffect(() => {
    if (!awaitingPayment || state?.paymentReady) return;
    let ticks = 0;
    const timer = setInterval(() => { setReload((value) => value + 1); if (++ticks >= 12) clearInterval(timer); }, 5000);
    return () => clearInterval(timer);
  }, [awaitingPayment, state?.paymentReady]);

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
  async function schedulePlan() {
    if (!workspaceID || !state?.periodEnd) return;
    setBusy(true); setError(""); setNotice("");
    try {
      await canterFetch("/workspaces/" + encodeURIComponent(workspaceID) + "/billing/plan-change", { method: "POST", body: JSON.stringify({ planId: selected, periodEnd: Math.floor(new Date(state.periodEnd).getTime() / 1000) }) });
      setNotice(selected === state.planId ? "Scheduled plan change canceled. Your current plan will continue." : "Your plan change is scheduled for renewal. Nothing was charged today.");
      setReload(value => value + 1);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Plan change could not be scheduled."); }
    finally { setBusy(false); }
  }
  const started = !!state && !["not_started", "canceled", "incomplete_expired"].includes(state.status);
  const currentPlan: PlanID = started && state?.planId === "pro" ? "pro" : "payg";
  const planName = currentPlan === "pro" ? "Pro" : "Pay as you go";
  const money = (cents: number) => (cents / 100).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  const title = view === "plans" ? "Plans" : view === "invoices" ? "Invoices" : "Usage";
  return <SettingsShell active={title} title={title} description={view === "plans" ? "Manage usage billing and your saved payment method." : view === "invoices" ? "Finalized invoices and payment details for your workspace." : "Your current plan, usage credit, and spending history."}><div className={styles.page}>
    {view === "usage" ? <div className={styles.tabs} role="tablist" aria-label="Usage views">{(["overview", "history"] as const).map(tab => <button key={tab} id={`${viewID}-${tab}-tab`} role="tab" aria-selected={usageTab === tab} aria-controls={`${viewID}-${tab}`} tabIndex={usageTab === tab ? 0 : -1} onClick={() => setUsageTab(tab)} onKeyDown={event => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const next = event.key === "Home" ? "overview" : event.key === "End" ? "history" : tab === "overview" ? "history" : "overview";
      setUsageTab(next); document.getElementById(`${viewID}-${next}-tab`)?.focus();
    }}>{tab === "overview" ? "Overview" : "Usage history"}</button>)}</div> : null}
    {error ? <div className={styles.notice} role="alert">{error} <button onClick={() => setReload((value) => value + 1)}>Retry</button></div> : null}
    {!state && !error ? <p className={styles.loading} role="status">Loading billing…</p> : null}
    {view === "usage" && state ? <>
      <div id={`${viewID}-overview`} role="tabpanel" aria-labelledby={`${viewID}-overview-tab`} hidden={usageTab !== "overview"}>
        <section className={styles.current} aria-label="Current plan"><div><span className={styles.badge}>Current plan</span><h2>{planName}</h2><p>{started ? `${state.planId === "pro" ? "$20/month" : "Usage-based billing"}${state.periodEnd ? ` · ${state.cancelAtPeriodEnd ? "Ends" : "Renews"} ${new Date(state.periodEnd).toLocaleDateString()}` : ""}` : "$0/month + resource usage. Add a payment method before deploying apps."}</p><div className={styles.planActions}><Link className={settings.primary} href="/app/billing?view=plans">{started ? "View billing" : "Add payment method"}</Link>{state.hasBillingAccount ? <button className={settings.button} disabled={busy || !state.checkoutEnabled || !canManage} onClick={() => openPayment("portal")}>Manage billing</button> : null}</div></div></section>
        {started && state.planId === "pro" ? <section className={styles.creditCard} aria-label="Included usage credit"><h2>Your included usage</h2><div><span>Monthly usage credit</span><strong>${money(state.bill.creditAppliedCents)} / $20.00</strong></div><progress aria-label="Monthly usage credit used" max={2000} value={state.bill.creditAppliedCents} /><p>${money(state.bill.creditRemainingCents)} remaining{state.periodEnd ? ` · Resets ${new Date(state.periodEnd).toLocaleDateString()}` : ""}</p></section> : null}
        <section className={styles.creditCard} aria-label="Period spending"><h2>This billing period</h2><div><span>Recorded resource usage</span><strong>${money(state.bill.usageCents)}</strong></div><div><span>Estimated total, before tax</span><strong>{started ? `$${money(state.bill.totalCents)}` : "Billing not started"}</strong></div><p>{started ? "Includes your subscription and recorded resource usage, after credits." : "No invoice has been started. Usage totals appear once billing is active."}</p></section>
        <p className={styles.small}>Your own OpenRouter key is billed directly by OpenRouter. That spending isn’t included in Canter’s resource usage.</p>
      </div>
      <div id={`${viewID}-history`} role="tabpanel" aria-labelledby={`${viewID}-history-tab`} hidden={usageTab !== "history"}>
        {!state.usage?.eventCount ? <div className={styles.historyEmpty}><h2>No recorded usage yet</h2><p>Spending will appear here as resource usage is recorded.</p></div> : null}
        {state.usage ? <BillingUsageView usage={state.usage} /> : null}
      </div>
    </> : null}
    {state?.paymentMethod ? <p className={styles.small}>{state.paymentMethod.brand.toUpperCase()} •••• {state.paymentMethod.last4}{state.paymentReady ? " · Ready for usage billing" : " · Payment setup needs attention"}</p> : null}
    {view === "invoices" && state ? <section className={styles.creditCard}><h2>{state.hasBillingAccount ? "Workspace invoices" : "No invoices yet"}</h2><p>{state.hasBillingAccount ? "View and download finalized invoices, receipts, and payment methods in the secure billing portal." : "Invoices will be available after you set up billing. Your usage estimate is not an invoice."}</p>{state.hasBillingAccount ? <button className={settings.primary} disabled={busy || !state.checkoutEnabled || !canManage} onClick={() => openPayment("portal")}>{busy ? "Opening…" : "Open invoices"}</button> : <Link className={settings.button} href="/app/billing?view=plans">View plans</Link>}{!canManage ? <p>Only a workspace owner can open billing documents.</p> : null}</section> : null}
    {view === "plans" ? <div>
    {state && !state.checkoutEnabled ? <div className={styles.notice} role="status">Payments aren’t available yet.</div> : null}
    {awaitingPayment && !state?.paymentReady ? <div className={styles.notice} role="status">Verifying your saved payment method. Adding a card does not deploy anything. <button onClick={() => setReload((value) => value + 1)}>Refresh</button></div> : null}
    {state && started ? <>
      <section className={styles.current}><div><span className="meta">Current plan</span><h2>{state.planId === "pro" ? "Pro" : "Pay as you go"}</h2><p>{state.status === "active" ? "Active" : state.status.replaceAll("_", " ")}{state.cancelAtPeriodEnd ? " · Ends at the close of this billing period" : ""}</p></div><button disabled={busy || !state.checkoutEnabled || !canManage} onClick={() => openPayment("portal")}>{busy ? "Opening…" : "Manage billing"}</button></section>
      <section className={styles.statement} aria-label="Current billing period"><header><h2>This billing period</h2><p>{state.periodStart ? new Date(state.periodStart).toLocaleDateString() : "—"} – {state.periodEnd ? new Date(state.periodEnd).toLocaleDateString() : "—"}</p></header>
        <dl><div><dt>Subscription</dt><dd>{dollars(state.bill.subscriptionCents)}</dd></div><div><dt>Recorded usage</dt><dd>{dollars(state.bill.usageCents)}</dd></div><div><dt>Usage credit applied</dt><dd>−{dollars(state.bill.creditAppliedCents)}</dd></div><div><dt>Additional usage</dt><dd>{dollars(state.bill.additionalUsageCents)}</dd></div><div className={styles.total}><dt>Estimated period total</dt><dd>{dollars(state.bill.totalCents)}</dd></div></dl>
        <p className={styles.small}>Before tax. This period’s total includes the subscription paid at the start. Usage may take time to arrive; finalized invoices are available under Manage billing.</p>
        {state.pendingEvents > 0 ? <p className={styles.small}>Some recorded usage is still syncing to your invoice.</p> : null}
        {state.reconciliationEvents > 0 ? <p className={styles.small}>Some usage needs reconciliation before the invoice can be confirmed.</p> : null}
      </section>
    </> : <section className={styles.current} aria-label="Current plan"><div><span className={styles.badge}>Current plan</span><h2>Pay as you go</h2><p>$0/month + resource usage. Add a payment method to deploy, or choose Pro below.</p></div></section>}
    {notice ? <p className={styles.notice} role="status">{notice}</p> : null}
    {state?.pendingPlanId && state.pendingPlanAt ? <p className={styles.notice} role="status">Switching to {state.pendingPlanId === "pro" ? "Pro ($20/month)" : "Pay as you go ($0/month + usage)"} on {new Date(state.pendingPlanAt).toLocaleDateString()}. Your current plan and included credit remain until then.</p> : null}
    <section className={styles.selection} aria-labelledby="choose-plan"><h2 id="choose-plan">Choose your plan</h2>
      <div className={styles.plans} role="group" aria-label="Billing plan">{(["payg", "pro"] as const).map(plan => <button key={plan} aria-pressed={selected === plan} onClick={() => setSelected(plan)}><span>{plan === "pro" ? "Pro" : "Pay as you go"}{currentPlan === plan ? " · Current plan" : ""}</span><strong>{plan === "pro" ? "$20" : "$0"}<small>/month</small></strong><p>{plan === "pro" ? "Includes $20 of infrastructure usage each month. Pay only for usage above that." : "No subscription fee. Pay for the resources your apps use."}</p></button>)}</div>
      <p className={styles.small}>{selected === "pro" ? "$20 is paid each month and deducted from that month’s usage bill. $21 of usage costs $21 total: $20 subscription plus $1 overage. Unused credit expires at renewal." : "Save a card securely with Stripe. Recorded resource usage is charged monthly, with no subscription fee."} All prices are USD, before tax.</p>
      {started ? <><p className={styles.small}>Plan changes take effect at your next renewal{state?.periodEnd ? ` on ${new Date(state.periodEnd).toLocaleDateString()}` : ""}. This month’s usage and credit stay on your current plan.</p><button className={styles.checkout} disabled={busy || !state?.checkoutEnabled || !canManage || !state?.periodEnd || state?.status !== "active" || state?.cancelAtPeriodEnd || (selected === currentPlan && !state?.pendingPlanId) || selected === state?.pendingPlanId} onClick={schedulePlan}>{busy ? "Saving…" : selected === currentPlan ? state?.pendingPlanId ? "Cancel scheduled plan change" : "Current plan" : selected === "pro" ? "Switch to Pro — $20/month at renewal" : "Switch to pay as you go at renewal"}</button></> : <button className={styles.checkout} disabled={busy || !state?.checkoutEnabled || !canManage} onClick={() => openPayment("checkout")}>{busy ? "Opening secure checkout…" : selected === "pro" ? "Subscribe to Pro — $20/month" : "Add payment method"}</button>}
    </section>
    {!started && state?.hasBillingAccount ? <p><button className={styles.checkout} disabled={busy || !state.checkoutEnabled || !canManage} onClick={() => openPayment("portal")}>Invoices & payment methods</button></p> : null}
    {!canManage ? <p className={styles.small}>Only a workspace owner can change the plan or manage payments.</p> : null}
    <p className={styles.small}><Link href="/pricing">View resource pricing ↗</Link></p>
    </div> : null}
  </div></SettingsShell>;
}

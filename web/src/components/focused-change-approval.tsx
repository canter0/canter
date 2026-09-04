"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { CanterAPIError, canterFetch, type ChangeApprovalResult, type ChangeApprovalReview } from "@/lib/canter-api";

export function FocusedChangeApproval({ token }: { token: string }) {
  const router = useRouter();
  const [review, setReview] = useState<ChangeApprovalReview | null>(null);
  const [result, setResult] = useState<ChangeApprovalResult | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const reviewPath = `/approve/change/${encodeURIComponent(token)}`;

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const loaded = await canterFetch<ChangeApprovalReview>(`/change-approvals/${encodeURIComponent(token)}`);
        if (!cancelled) setReview(loaded);
      } catch (cause) {
        if (cause instanceof CanterAPIError && cause.status === 401) {
          router.replace(`/sign-in?next=${encodeURIComponent(reviewPath)}`);
          return;
        }
        if (!cancelled) setError(cause instanceof Error ? cause.message : "This review route is unavailable.");
      }
    })();
    return () => { cancelled = true; };
  }, [reviewPath, router, token]);

  async function approve() {
    if (!review || !review.canApprove) return;
    setPending(true);
    setError("");
    try {
      const accepted = await canterFetch<ChangeApprovalResult>(`/change-approvals/${encodeURIComponent(token)}/approve`, { method: "POST", body: "{}" });
      setResult(accepted);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not record this approval.");
    } finally {
      setPending(false);
    }
  }

  const change = review?.change;
  const impact = change?.plan?.impact;
  const assessment = change ? [
    ["digest", change.digest],
    ["system", review.capability.system],
    ["base release", change.plan?.baseVersion ?? "none"],
    ["availability", impact?.availability ?? "not assessed"],
    ["data", impact?.data ?? "not assessed"],
    ["monthly cost", impact?.monthlyCostDeltaCents == null ? "not assessed" : `${impact.monthlyCostDeltaCents >= 0 ? "+" : "−"}$${Math.abs(impact.monthlyCostDeltaCents / 100).toFixed(2)}`],
    ["expires", new Date(review.capability.expiresAt).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })],
  ] : [];

  return (
    <main className="flex min-h-screen flex-col px-6 sm:px-10 lg:px-16">
      <header className="flex h-24 items-center"><Link href="/" className="wordmark text-[24px] leading-none">canter</Link></header>
      <section className="mx-auto flex w-full max-w-[920px] flex-1 flex-col justify-center py-16">
        {result ? (
          <div className="max-w-[680px]">
            <span className="signal mb-8" />
            <div className="meta">APPROVAL RECORDED</div>
            <h1 className="display mt-4 text-[clamp(40px,5vw,68px)] leading-[0.96] tracking-[-0.045em]">The exact Change is executing.</h1>
            <div className="mt-12 border-y border-[var(--ink)] py-6">
              <div className="flex justify-between gap-8"><span>execution</span><span className="text-right text-[var(--muted)]">{result.execution.id}</span></div>
              <div className="mt-4 flex justify-between gap-8"><span>digest</span><span className="max-w-[480px] break-all text-right text-[10px] text-[var(--muted)]">{result.capability.digest}</span></div>
            </div>
            <p className="mt-8 text-[var(--muted)]">Return to your agent. Canter retained the human identity, exact digest, execution, and evidence in the ledger.</p>
          </div>
        ) : (
          <>
            <div className="border-b border-[var(--ink)] pb-8">
              <div className="meta">FOCUSED CHANGE REVIEW · REQUESTED BY {review?.capability.requestedBy.name ?? "AGENT"}</div>
              <h1 className="display mt-4 max-w-[780px] text-[clamp(34px,4.2vw,58px)] leading-[1.02] tracking-[-0.04em]">{change?.summary ?? (error ? "This review route cannot be used." : "Loading exact Change.")}</h1>
            </div>
            {error ? <div role="alert" className="mt-8 border-l-2 border-[var(--ink)] pl-4 leading-5">{error}</div> : null}
            {review ? <div className="mt-10 grid gap-x-16 gap-y-12 lg:grid-cols-[1.15fr_1fr]">
              <div>
                <div className="meta border-b border-[var(--rule)] pb-3">EXECUTION PLAN · {change?.operations?.length ?? 0} STEPS</div>
                {change?.operations?.map((operation, index) => <div key={operation.id} className="grid min-h-14 grid-cols-[44px_1fr] items-center border-b border-[var(--rule)] py-3"><span className="text-[var(--muted)]">{String(index + 1).padStart(2, "0")}</span><div><div>{operation.description}</div><div className="mt-1 text-[var(--muted)]">{operation.kind}</div></div></div>)}
              </div>
              <div>
                <div className="meta border-b border-[var(--rule)] pb-3">EXACT BOUNDARY</div>
                {assessment.map(([key, value]) => <div key={key} className="flex min-h-12 items-center justify-between gap-8 border-b border-[var(--rule)] py-3"><span>{key}</span><span className={`max-w-[270px] break-all text-right text-[var(--muted)] ${key === "digest" ? "text-[9px]" : ""}`}>{value}</span></div>)}
              </div>
            </div> : null}
            {review ? <div className="mt-14 flex flex-col items-start justify-between gap-6 border-t border-[var(--ink)] pt-7 sm:flex-row sm:items-center"><p className="max-w-[520px] text-[var(--muted)]">This route can authorize and enqueue only the digest shown above. It cannot grant the agent broader permission and cannot be replayed.</p><button onClick={() => void approve()} disabled={pending || !review.canApprove} className="h-11 min-w-[230px] bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-50">{pending ? "Recording…" : review.canApprove ? "Approve + apply exact Change" : "Approval unavailable"}</button></div> : null}
          </>
        )}
      </section>
    </main>
  );
}

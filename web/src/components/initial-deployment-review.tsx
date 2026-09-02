"use client";

import { useEffect, useState } from "react";
import { AppShell, SectionHeader } from "@/components/app-shell";
import { canterFetch, relativeTime, type InitialDeploymentDetail, type InitialDeploymentExecution, type Me } from "@/lib/canter-api";

const terminalPhases = new Set(["succeeded", "failed"]);

export function InitialDeploymentReview({ id }: { id: string }) {
  const [workspaceId, setWorkspaceId] = useState("");
  const [deployment, setDeployment] = useState<InitialDeploymentDetail | null>(null);
  const [execution, setExecution] = useState<InitialDeploymentExecution | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const me = await canterFetch<Me>("/me");
        const workspace = me.workspaces[0];
        if (!workspace) throw new Error("No workspace is available.");
        const result = await canterFetch<InitialDeploymentDetail>(`/workspaces/${workspace.id}/initial-deployments/${encodeURIComponent(id)}`);
        if (cancelled) return;
        setWorkspaceId(workspace.id);
        setDeployment(result);
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load this proposal.");
      }
    })();
    return () => { cancelled = true; };
  }, [id]);

  useEffect(() => {
    if (!workspaceId || !deployment || terminalPhases.has(deployment.phase) || !["queued", "running"].includes(deployment.phase)) return;
    const timer = window.setInterval(() => {
      void (async () => {
        try {
          const [nextDeployment, nextExecution] = await Promise.all([
            canterFetch<InitialDeploymentDetail>(`/workspaces/${workspaceId}/initial-deployments/${encodeURIComponent(id)}`),
            execution ? canterFetch<InitialDeploymentExecution>(`/initial-deployment-executions/${encodeURIComponent(execution.id)}`) : Promise.resolve(null),
          ]);
          setDeployment(nextDeployment);
          if (nextExecution) setExecution(nextExecution);
        } catch (cause) {
          setError(cause instanceof Error ? cause.message : "Canter could not refresh execution state.");
        }
      })();
    }, 2_000);
    return () => window.clearInterval(timer);
  }, [deployment, execution, id, workspaceId]);

  async function authorize() {
    if (!deployment || !workspaceId) return;
    setPending(true);
    setError("");
    try {
      const result = await canterFetch<InitialDeploymentDetail>(`/workspaces/${workspaceId}/initial-deployments/${encodeURIComponent(id)}/authorize`, {
        method: "POST",
        body: JSON.stringify({ digest: deployment.digest }),
      });
      setDeployment(result);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not authorize this proposal.");
    } finally {
      setPending(false);
    }
  }

  async function apply() {
    if (!deployment || !workspaceId) return;
    setPending(true);
    setError("");
    try {
      const result = await canterFetch<InitialDeploymentExecution>(`/workspaces/${workspaceId}/initial-deployments/${encodeURIComponent(id)}/apply`, { method: "POST" });
      setExecution(result);
      setDeployment({ ...deployment, phase: "queued" });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not enqueue this deployment.");
    } finally {
      setPending(false);
    }
  }

  const plan = deployment?.plan;
  const host = plan?.system.spec.constraints?.host;
  const services = plan?.system.spec.services ?? [];
  const authorization = deployment?.authorization;
  const canRetry = deployment?.phase === "failed" && authorization?.digest === deployment.digest;
  const assessment: Array<[string, string]> = deployment && plan ? [
    ["digest", deployment.digest],
    ["artifact", plan.artifactSha256],
    ["workspace revision", String(plan.workspaceRevision)],
    ["System", plan.system.metadata.name],
    ["host", host ? `${host.class ?? "compute"} × ${host.count ?? 1} · ${host.memoryMiB ?? "—"} MiB` : "not declared"],
    ["services", services.map((service) => `${service.name} × ${service.instances ?? 1}`).join(", ") || "none"],
    ["command", plan.release.command.join(" ")],
    ["public port", String(plan.release.publicPort)],
    ["health", plan.release.healthPath],
    ["verification", `${plan.verification.method || "GET"} ${plan.verification.path} → ${plan.verification.expectedStatus}`],
    ["authorized by", authorization?.authorizedBy?.displayName ?? authorization?.authorizedBy?.id ?? "not authorized"],
  ] : [];

  return (
    <AppShell active="Changes" context={`canter / default / ${id}`}>
      <section className="flex min-h-[calc(100vh-80px)] flex-col overflow-x-auto px-6 pt-10 sm:px-10 lg:px-14 lg:pt-11">
        <div className="grid min-w-[760px] grid-cols-[1fr_210px] items-end border-b border-[var(--ink)] pb-7">
          <div>
            <div className="meta">INITIAL DEPLOYMENT · {id} · DRAFTED BY {deployment?.draftedBy.displayName ?? deployment?.draftedBy.id ?? "AGENT"}</div>
            <h1 className="display mt-3 text-[32px]">{deployment?.summary ?? "Loading proposal"}</h1>
          </div>
          <div className="flex items-center justify-end gap-2 pb-1 text-[10px]"><span className={`signal ${deployment?.phase === "failed" ? "opacity-30" : ""}`} />{deployment?.phase.toUpperCase() ?? "LOADING"}</div>
        </div>

        {error ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{error}</div> : null}
        {deployment?.failure ? <div className="mt-8 border-l-2 border-[var(--ink)] pl-4">{deployment.failure}</div> : null}

        <div className="mt-10 grid min-w-[760px] gap-12 lg:grid-cols-[1.35fr_1fr]">
          <div>
            <SectionHeader left="EXACT EXECUTION PLAN" right={`${deployment?.operations.length ?? 0} STEPS`} />
            {deployment?.operations.map((operation, index) => (
              <div key={operation.id} className="grid min-h-14 grid-cols-[56px_1fr_120px] items-center border-b border-[var(--rule)] py-3">
                <span className="text-[var(--muted)]">{String(index + 1).padStart(2, "0")}</span>
                <div><div>{operation.description}</div><div className="mt-1 text-[var(--muted)]">{operation.kind}{operation.failure ? ` · ${operation.failure}` : ""}</div></div>
                <span className="text-right text-[var(--muted)]">{operation.phase}</span>
              </div>
            ))}
          </div>
          <div>
            <SectionHeader left="EXACT ASSESSMENT" />
            {assessment.map(([key, value]) => <div key={key} className="flex min-h-11 items-center justify-between gap-8 border-b border-[var(--rule)] py-3"><span>{key}</span><span className={`max-w-[260px] break-all text-right text-[var(--muted)] ${key === "digest" || key === "artifact" ? "text-[9px]" : ""}`}>{value}</span></div>)}
          </div>
        </div>

        <div className="mt-12 min-w-[760px]">
          <SectionHeader left="EVIDENCE" right={`${deployment?.evidence?.length ?? 0} RECORDS`} />
          {deployment?.evidence?.map((record) => <div key={`${record.operationId}-${record.observedAt}`} className="grid min-h-12 grid-cols-[170px_1fr_140px] items-center border-b border-[var(--rule)] py-3"><span className="text-[var(--muted)]">{record.operationId}</span><span>{record.statement}</span><span className="text-right text-[var(--muted)]">{relativeTime(record.observedAt)}</span></div>)}
          {deployment && !deployment.evidence?.length ? <div className="flex h-16 items-center border-b border-[var(--rule)] text-[var(--muted)]">No execution evidence yet.</div> : null}
        </div>

        {execution ? <div className="mt-8 border-l-2 border-[var(--signal)] pl-4">Execution {execution.id} · {execution.phase} · attempt {execution.attempts}</div> : null}

        <div className="mt-auto flex min-h-20 min-w-[760px] items-center justify-end gap-3 border-t border-[var(--ink)]">
          {deployment?.phase === "drafted" ? <button onClick={() => void authorize()} disabled={pending} className="flex h-10 min-w-[190px] items-center justify-center bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-50">{pending ? "Authorizing…" : "Authorize exact digest"}</button> : null}
          {deployment?.phase === "authorized" ? <button onClick={() => void apply()} disabled={pending} className="flex h-10 min-w-[190px] items-center justify-center bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-50">{pending ? "Enqueuing…" : "Apply approved proposal"}</button> : null}
          {canRetry ? <button onClick={() => void apply()} disabled={pending} className="flex h-10 min-w-[190px] items-center justify-center bg-[var(--ink)] px-6 text-[var(--paper)] disabled:opacity-50">{pending ? "Retrying…" : "Retry failed proposal"}</button> : null}
        </div>
      </section>
    </AppShell>
  );
}

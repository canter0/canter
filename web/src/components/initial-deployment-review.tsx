"use client";
import {getSelectedWorkspace} from "@/lib/workspace-selection";

import { useEffect, useState } from "react";
import { useSurfaceWorkspace } from "./embedded-app-surface";
import { DeploymentPanel } from "./deployment-panel";
import { AppShell } from "@/components/app-shell";
import styles from "./detail-page.module.css";
import { canterFetch, type InitialDeploymentDetail, type InitialDeploymentExecution } from "@/lib/canter-api";

const terminalPhases = new Set(["succeeded", "failed"]);

export function InitialDeploymentReview({ id }: { id: string }) {
  const scopedWorkspace = useSurfaceWorkspace();
  const [workspaceId, setWorkspaceId] = useState("");
  const [deployment, setDeployment] = useState<InitialDeploymentDetail | null>(null);
  const [execution, setExecution] = useState<InitialDeploymentExecution | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const workspace = scopedWorkspace ? { id: scopedWorkspace } : (await getSelectedWorkspace()).workspace;
        if (!workspace) throw new Error("No workspace is available.");
        const result = await canterFetch<InitialDeploymentDetail>(`/workspaces/${workspace.id}/initial-deployments/${encodeURIComponent(id)}`);
        if (cancelled) return;
        setWorkspaceId(workspace.id);
        setDeployment(result);
        setError("");
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "Canter could not load this proposal.");
      }
    })();
    return () => { cancelled = true; };
  }, [id, scopedWorkspace]);

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
          setError("");
          if (nextExecution) setExecution(nextExecution);
        } catch (cause) {
          setError(cause instanceof Error ? cause.message : "Canter could not refresh execution state.");
        }
      })();
    }, 2_000);
    return () => window.clearInterval(timer);
  }, [deployment, execution, id, workspaceId]);

  async function authorizeAndApply() {
    if (!deployment || !workspaceId || pending) return;
    setPending(true);
    setError("");
    try {
      const authorized = await canterFetch<InitialDeploymentDetail>(`/workspaces/${workspaceId}/initial-deployments/${encodeURIComponent(id)}/authorize`, {
        method: "POST",
        body: JSON.stringify({ digest: deployment.digest }),
      });
      setDeployment(authorized);
      const result = await canterFetch<InitialDeploymentExecution>(`/workspaces/${workspaceId}/initial-deployments/${encodeURIComponent(id)}/apply`, { method: "POST" });
      setExecution(result);
      setDeployment({ ...authorized, phase: "queued" });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Canter could not approve and start this deployment.");
    } finally {
      setPending(false);
    }
  }

  async function apply() {
    if (!deployment || !workspaceId || pending) return;
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
  const authorization = deployment?.authorization;
  const unsupportedClassFailure = [deployment?.failure, ...(deployment?.operations.map((operation) => operation.failure) ?? [])]
    .find((failure) => failure?.startsWith("unsupported_compute_class:"));
  const canRetry = deployment?.phase === "failed" && authorization?.digest === deployment.digest && !unsupportedClassFailure;
  const actionExplanation = deployment?.phase === "drafted"
    ? "Records approval of this exact digest, then starts that unchanged deployment."
    : deployment?.phase === "authorized"
      ? "Approval is recorded. Starting creates the server-owned execution."
      : unsupportedClassFailure
        ? `This immutable proposal used ${host?.class ?? "an unsupported class"}. Ask the agent to draft a corrected c1 proposal; this record remains failed history.`
      : canRetry
        ? "Retries the same approved digest; it does not broaden the proposal."
        : "";
  const panel = <DeploymentPanel deployment={deployment} execution={execution} error={error} pending={pending} canRetry={!!canRetry} explanation={actionExplanation} onApprove={() => void authorizeAndApply()} onApply={() => void apply()} />;
  if (scopedWorkspace) return panel;

  return (
    <AppShell active="Changes" context={`canter / default / ${id}`} pageTitle={deployment?.summary ?? "Deployment review"}>
      <section className={`${styles.page} ${styles.deployment}`}>{panel}</section>
    </AppShell>
  );
}

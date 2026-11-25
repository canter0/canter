"use client";

import { useEffect, useRef, useState, type FormEvent } from "react";
import { canterFetch, type Workspace } from "@/lib/canter-api";
import { openSelectedWorkspace, selectWorkspace } from "@/lib/workspace-selection";
import { WorkspaceIcon } from "./workspace-icon";
import { useDialog } from "./use-dialog";
import styles from "./agent-connection-dialog.module.css";

type Phase = "creating" | "opening";

export function CreateOrganizationDialog({ onClose }: { onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [name, setName] = useState("");
  const [phase, setPhase] = useState<Phase | null>(null);
  const [error, setError] = useState("");
  const request = useRef<{ name: string; id: string } | null>(null);
  const active = useRef<AbortController | null>(null);
  const dismiss = useDialog(dialog, "input");

  useEffect(() => () => active.current?.abort(), []);

  function close() {
    active.current?.abort();
    dismiss(onClose);
  }

  async function create(event: FormEvent) {
    event.preventDefault();
    const trimmed = name.trim();
    if (active.current || !trimmed) return;

    const controller = new AbortController();
    const timeout = AbortSignal.timeout(20_000);
    const signal = AbortSignal.any([controller.signal, timeout]);
    active.current = controller;
    setPhase("creating");
    setError("");

    try {
      if (request.current?.name !== trimmed) {
        request.current = { name: trimmed, id: `wrk_${crypto.randomUUID()}` };
      }
      const workspace = await canterFetch<Workspace>("/workspaces", {
        method: "POST",
        signal,
        body: JSON.stringify(request.current),
      });
      signal.throwIfAborted();
      setPhase("opening");
      await selectWorkspace(workspace.id, signal);
      signal.throwIfAborted();
      openSelectedWorkspace();
      dialog.current?.close();
      onClose();
    } catch (cause) {
      if (controller.signal.aborted) return;
      setError(timeout.aborted
        ? "The request timed out. Try again."
        : cause instanceof Error ? cause.message : "Couldn’t create this organization.");
      active.current = null;
      setPhase(null);
    }
  }

  return <dialog ref={dialog} className={styles.dialog} aria-labelledby="create-organization-title" onCancel={event => { event.preventDefault(); close(); }}>
    <button type="button" className={styles.close} aria-label="Close" onClick={close}><WorkspaceIcon name="close" width="17" height="17" /></button>
    <div className={styles.icon}><WorkspaceIcon name="plus" width="23" height="23" /></div>
    <h2 id="create-organization-title">Create an organization</h2>
    <form onSubmit={create}>
      <p>Organizations keep their own people, agents, and work.</p>
      <label className={styles.field}>Name<input type="text" required maxLength={64} readOnly={phase !== null} value={name} onChange={event => setName(event.target.value)} placeholder="Acme team" /></label>
      <button type="submit" className={styles.primary} disabled={phase !== null || !name.trim()}>{phase === "creating" ? "Creating…" : phase === "opening" ? "Opening…" : "Create organization"}</button>
    </form>
    {error ? <p className={styles.error} role="alert">{error}</p> : null}
  </dialog>;
}

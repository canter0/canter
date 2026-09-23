"use client";

import { useEffect, useRef, useState } from "react";
import { canterFetch, type Authority } from "@/lib/canter-api";
import { AgentPermissions } from "./agent-permissions";
import { WorkspaceIcon } from "./workspace-icon";
import styles from "./agent-connection-dialog.module.css";

type Pairing = {
  id: string;
  token?: string;
  status: "waiting" | "ready" | "approved" | "connected" | "expired" | "cancelled" | "disconnected";
  name?: string;
  harness?: string;
  expiresAt: string;
};

export function AgentConnectionDialog({ workspaceId, defaultAuthority, onClose, onConnected }: { workspaceId: string; defaultAuthority: Authority; onClose: () => void; onConnected: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const callbacks = useRef({ onClose, onConnected });
  useEffect(() => { callbacks.current = { onClose, onConnected }; }, [onClose, onConnected]);
  const [pairing, setPairing] = useState<Pairing | null>(null);
  const [prompt, setPrompt] = useState("");
  const [copied, setCopied] = useState(false);
  const [manualCopy, setManualCopy] = useState(false);
  const [override, setOverride] = useState<Authority | null>(null);
  const authority = override ?? defaultAuthority;
  const [remember, setRemember] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const id = pairing?.id;
  const terminal = pairing && ["connected", "expired", "cancelled", "disconnected"].includes(pairing.status);

  useEffect(() => { dialog.current?.showModal(); }, []);
  useEffect(() => {
    if (!id || terminal) return;
    const controller = new AbortController();
    let running = false;
    async function poll() {
      if (running || controller.signal.aborted) return;
      running = true;
      try {
        const next = await canterFetch<Pairing>(`/agent-pairings/${encodeURIComponent(id!)}`, { signal: controller.signal });
        if (controller.signal.aborted) return;
        setPairing(current => ({ ...current, ...next }));
        setError("");
        if (next.status === "connected") callbacks.current.onConnected();
      } catch { if (!controller.signal.aborted) setError("Connection interrupted. Retrying…"); }
      finally { running = false; }
    }
    const interval = window.setInterval(() => void poll(), 1500);
    void poll();
    return () => { controller.abort(); window.clearInterval(interval); };
  }, [id, terminal]);

  async function copyPrompt() {
    if (pending) return;
    setPending(true); setError("");
    try {
      let text = prompt;
      if (!pairing) {
        const next = await canterFetch<Pairing>("/agent-pairings", { method: "POST", body: JSON.stringify({ workspaceId }) });
        const base = `${window.location.origin}/api/canter`;
        text = `Connect to my Canter workspace. Read the connection instructions at ${base}/agent-pairings/instructions and use this one-time pairing code: ${next.token}. Identify yourself with a name I will recognize, then wait for me to click Connect in Canter. Keep all returned credentials private. If you use subagents, give them scoped worker access through your connection.`;
        setPairing(next); setPrompt(text);
      }
      try { await navigator.clipboard.writeText(text); setCopied(true); setManualCopy(false); }
      catch { setManualCopy(true); }
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t start the connection. Try again."); }
    finally { setPending(false); }
  }

  async function close() {
    if (pending) return;
    setPending(true); setError("");
    try {
      if (pairing && !["connected", "disconnected", "cancelled", "expired"].includes(pairing.status)) await canterFetch(`/agent-pairings/${encodeURIComponent(pairing.id)}`, { method: "DELETE" });
      callbacks.current.onClose();
    } catch { setError("Couldn’t cancel the connection. Try again."); setPending(false); }
  }

  async function approve() {
    if (!pairing || pending) return;
    setPending(true); setError("");
    try {
      await canterFetch(`/agent-pairings/${encodeURIComponent(pairing.id)}/approve`, { method: "POST", body: JSON.stringify({ remember, ...(override ? { authority: override } : {}) }) });
      setPairing(current => current ? { ...current, status: "approved" } : current);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Couldn’t connect. Try again."); }
    finally { setPending(false); }
  }

  const name = pairing?.name || "Your agent";
  const ended = pairing && ["expired", "cancelled", "disconnected"].includes(pairing.status);
  return <dialog ref={dialog} className={styles.dialog} aria-labelledby="connect-agent-title" onCancel={event => { event.preventDefault(); void close(); }}>
    <button className={styles.close} aria-label="Close agent connection" disabled={pending} onClick={() => void close()}><WorkspaceIcon name="close" width="17" height="17" /></button>
    <div className={styles.icon}><WorkspaceIcon name={pairing?.status === "connected" ? "check" : "terminal"} width="23" height="23" /></div>
    <h2 id="connect-agent-title">{pairing?.status === "connected" ? `${name} connected` : pairing?.status === "ready" ? `${name} is ready to connect` : ended ? "Connection ended" : "Connect your agent"}</h2>
    {!pairing || pairing.status === "waiting" ? <>
      <p>Paste this message into your agent.</p>
      <p>{override ? "Custom permissions for this agent." : "Using workspace defaults."}{override ? <button type="button" onClick={() => setOverride(null)} disabled={pending}> Use workspace defaults</button> : null}</p>
      <AgentPermissions value={authority} onChange={setOverride} disabled={pending} />
      <button className={styles.primary} disabled={pending} onClick={() => void copyPrompt()}><WorkspaceIcon name={copied ? "check" : "copy"} width="15" height="15" />{pending ? "Preparing…" : copied ? "Copy again" : "Copy connection prompt"}</button>
      {manualCopy ? <><p>Copy this message:</p><textarea aria-label="Connection prompt" readOnly value={prompt} onFocus={event => event.currentTarget.select()} /></> : null}
      {pairing ? <p className={styles.waiting} role="status"><span />Waiting for your agent…</p> : null}
    </> : pairing.status === "ready" ? <>
      <p>{pairing.harness}</p>
      <p>{override ? "Custom permissions for this agent." : "Using workspace defaults."}{override ? <button type="button" onClick={() => setOverride(null)} disabled={pending}> Use workspace defaults</button> : null}</p>
      <AgentPermissions value={authority} onChange={setOverride} disabled={pending} />
      <label className={styles.remember}><input type="checkbox" checked={remember} onChange={event => setRemember(event.target.checked)} disabled={pending} />Remember this agent</label>
      <details className={styles.details}><summary>Connection details</summary><p>{remember ? "This agent can reconnect until you disconnect it." : "Access ends after its task, or after 8 hours."} Workers stay under this connection.</p></details>
      <button className={styles.primary} disabled={pending} onClick={() => void approve()}>{pending ? "Connecting…" : "Connect"}</button>
    </> : pairing.status === "approved" ? <p className={styles.waiting} role="status"><span />Finishing the connection…</p> : pairing.status === "connected" ? <><p>You’ll see its actions in Activity.</p><button className={styles.primary} onClick={() => void close()}>Done</button></> : <><p>{pairing.status === "expired" ? "The invitation expired. Copy a new one to try again." : "Start again whenever you’re ready."}</p><button className={styles.primary} onClick={() => { setPairing(null); setPrompt(""); setCopied(false); setManualCopy(false); setRemember(false); setError(""); }}>Start again</button></>}
    {error ? <p className={styles.error} role="alert">{error}</p> : null}
  </dialog>;
}

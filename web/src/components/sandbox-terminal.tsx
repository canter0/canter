"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { canterFetch } from "@/lib/canter-api";
import styles from "./workspace.module.css";

// SandboxTerminal streams a guest bash session over the control plane's
// terminal endpoints. Output is newline-delimited base64 chunks.
export function SandboxTerminal({ base, interactive }: { base: string; interactive: boolean }) {
  const host = useRef<HTMLDivElement | null>(null);
  const terminal = useRef<Terminal | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const session = useRef<string | null>(null);
  const pending = useRef<string>("");
  const flushing = useRef<ReturnType<typeof setInterval> | null>(null);
  const alive = useRef(false);
  const [exited, setExited] = useState(false);
  const [connectError, setConnectError] = useState("");

  const send = useCallback(() => {
    if (!session.current || !pending.current) return;
    const data = pending.current;
    pending.current = "";
    void fetch(`/api/canter${base}/terminals/${session.current}/input`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ d: btoa(unescape(encodeURIComponent(data))) }),
    }).catch(() => {});
  }, [base]);

  const stop = useCallback(async () => {
    alive.current = false;
    if (flushing.current) { window.clearInterval(flushing.current); flushing.current = null; }
    if (session.current) {
      await canterFetch(`${base}/terminals/${session.current}`, { method: "DELETE" }).catch(() => {});
      session.current = null;
    }
  }, [base]);

  const connect = useCallback(async () => {
    if (!terminal.current) return;
    setConnectError("");
    setExited(false);
    try {
      const created = await canterFetch<{ id: string }>(`${base}/terminals`, { method: "POST", body: "{}" });
      session.current = created.id;
      alive.current = true;
      flushing.current = setInterval(send, 30);
      const response = await fetch(`/api/canter${base}/terminals/${created.id}/output`, { credentials: "include" });
      if (!response.ok || !response.body) throw new Error("The terminal stream did not open.");
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffered = "";
      while (alive.current) {
        const { value, done } = await reader.read();
        if (done) break;
        buffered += decoder.decode(value, { stream: true });
        let index;
        while ((index = buffered.indexOf("\n")) >= 0) {
          const line = buffered.slice(0, index);
          buffered = buffered.slice(index + 1);
          if (!line.trim()) continue;
          try {
            const frame = JSON.parse(line) as { d?: string; exit?: boolean };
            if (frame.d) terminal.current?.write(decodeURIComponent(escape(atob(frame.d))));
            if (frame.exit) { setExited(true); session.current = null; }
          } catch { /* partial JSON lines are dropped */ }
        }
      }
      setExited(true);
    } catch (cause) {
      setConnectError(cause instanceof Error ? cause.message : "Could not open the terminal.");
      setExited(true);
    }
  }, [base, send]);

  useEffect(() => {
    if (!host.current || terminal.current) return;
    const term = new Terminal({ cursorBlink: true, fontSize: 13, theme: { background: "#141414" }, convertEol: false });
    const addon = new FitAddon();
    term.loadAddon(addon);
    term.open(host.current);
    addon.fit();
    terminal.current = term;
    fit.current = addon;
    term.onData((data) => { if (!interactive) return; pending.current += data; });
    term.onResize(({ cols, rows }) => {
      if (!session.current) return;
      void fetch(`/api/canter${base}/terminals/${session.current}/resize`, {
        method: "POST", headers: { "content-type": "application/json" }, credentials: "include",
        body: JSON.stringify({ cols, rows }),
      }).catch(() => {});
    });
    const observer = new ResizeObserver(() => { try { fit.current?.fit(); } catch { /* hidden */ } });
    if (host.current) observer.observe(host.current);
    return () => { observer.disconnect(); };
  }, [base, interactive]);

  useEffect(() => {
    queueMicrotask(() => void connect());
    return () => { void stop(); };
  }, [connect, stop]);

  return <div>
    <div ref={host} style={{ height: 360, background: "#141414", border: "1px solid #2c2c2c", borderRadius: 8, padding: 8 }} />
    <div style={{ marginTop: 8, display: "flex", gap: 8, alignItems: "center" }}>
      {connectError ? <span role="alert" className={styles.error}>{connectError}</span> : null}
      {exited ? <button className={styles.secondaryButton} onClick={() => void connect()}>New session</button> : null}
      {!interactive ? <span className={styles.status}>read-only</span> : null}
    </div>
  </div>;
}

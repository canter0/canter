"use client";
import Link from "next/link";
import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Quicknav } from "./quicknav";
import { useMediaQuery } from "./use-media-query";
import { useFocusContainment } from "./use-focus-containment";
import { WrenSidebar } from "./wrens-workspace";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon } from "./workspace-icon";
import {
  selectWorkspace,
  openSelectedWorkspace,
} from "@/lib/workspace-selection";
import styles from "./wren-navigation.module.css";

/** Mounted once in the route layout: chat navigation keeps its DOM and scroll. */
export function WrenNavigation() {
  const { data } = useWorkspace();
  const [error, setError] = useState("");
  const [open, setOpen] = useState(false);
  const mobile = useMediaQuery("(max-width: 1023px)");
  const panel = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const backdrop = useRef<HTMLDivElement>(null);
  const id = useId();
  const close = useCallback(() => setOpen(false), []);
  const modal = mobile && open;
  useFocusContainment(modal, panel, close, trigger, backdrop);
  useEffect(() => {
    const media = window.matchMedia("(max-width: 1023px)");
    const resize = () => {
      close();
      if (media.matches && panel.current?.contains(document.activeElement)) trigger.current?.focus();
    };
    media.addEventListener("change", resize);
    return () => media.removeEventListener("change", resize);
  }, [close]);
  return <>
    <button ref={trigger} type="button" className={`dashboard-theme ${styles.trigger}`} aria-label="Open navigation" aria-expanded={modal} aria-controls={id} onClick={() => setOpen(true)}><WorkspaceIcon name="panel" /></button>
    <div ref={backdrop} className={styles.backdrop} data-open={modal} aria-hidden="true" onClick={close} />
    <div ref={panel} id={id} className={`dashboard-theme ${styles.panel}`} data-open={open} inert={mobile && !open} role={modal ? "dialog" : undefined} aria-modal={modal || undefined} aria-label="Workspace navigation" tabIndex={-1} onClick={event => { if (event.target instanceof Element && event.target.closest("a")) close(); }}>
    <Quicknav />
    <aside
      className={`dashboard-theme ${styles.navigation}`}
      aria-label="Conversations"
    >
      <header>
        <Link
          href="/app/wrens"
          className="wordmark"
          aria-label="Conversations home"
        >
          canter
        </Link>
        <button type="button" className={styles.close} aria-label="Close navigation" onClick={close}><WorkspaceIcon name="close" /></button>
      </header>
      <div className={styles.list}>
        <WrenSidebar />
      </div>
      <footer>
        <label>
          <span className={styles.srOnly}>Workspace</span>
          <select
            aria-label="Workspace"
            value={data?.workspace.id ?? ""}
            onChange={async (event) => {
              try {
                setError("");
                await selectWorkspace(event.target.value);
                openSelectedWorkspace();
              } catch {
                setError("Couldn’t switch workspaces. Try again.");
              }
            }}
          >
            {(data?.workspaces ?? (data ? [data.workspace] : [])).map(
              (workspace) => (
                <option key={workspace.id} value={workspace.id}>
                  {workspace.name}
                </option>
              ),
            )}
          </select>
        </label>
        <Link href="/app/account" aria-label="Your account">
          <WorkspaceIcon name="profile" width="18" height="18" />
        </Link>
      </footer>
      {error ? <p role="alert">{error}</p> : null}
    </aside>
    </div>
  </>;
}

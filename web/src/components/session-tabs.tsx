"use client";
import { useLayoutEffect, useRef, type ReactNode } from "react";
import { useSessionTabs } from "./session-tabs-context";
import { useWorkspace } from "./workspace-context";
import { WorkspaceIcon, type WorkspaceIconName } from "./workspace-icon";
import { WrenMark } from "./wren-mark";
import type { Conversation } from "@/lib/operator-api";
import styles from "./workspace.module.css";

const sectionIcons: Array<[RegExp, WorkspaceIconName]> = [
  [/^\/app\/system/, "apps"],
  [/^\/app\/changes/, "activity"],
  [/^\/app\/agents/, "agent"],
  [/^\/app\/settings/, "settings"],
  [/^\/app\/account/, "profile"],
  [/^\/app\/billing/, "usage"],
  [/^\/app\/customize/, "customize"],
  [/^\/app\/products/, "apps"],
  [/^\/app\/sandboxes/, "terminal"],
  [/^\/app\/ci/, "changes"],
  [/^\/app\/tasks/, "message"],
];

function tabGlyph(path: string, running: boolean): ReactNode {
  if (path === "/app") return <WorkspaceIcon name="home" width="14" height="14" />;
  const conversation = /^\/app\/conversations\/[^/]+/.test(path);
  if (conversation) return running ? <span className={styles.sessionTabSpinner} /> : <span className={styles.sessionTabDot} />;
  if (/^\/app\/wrens/.test(path)) return <WrenMark width="14" height="14" />;
  const icon = sectionIcons.find(([pattern]) => pattern.test(path))?.[1];
  return icon ? <WorkspaceIcon name={icon} width="14" height="14" /> : <span className={styles.sessionTabDot} />;
}

// Browser-style workspace tab strip. All state and navigation semantics live in
// the WorkspaceProvider session-tabs controller; this component only renders it.
export function SessionTabs() {
  const controller = useSessionTabs();
  const { data } = useWorkspace();
  const conversations = data?.conversations;
  const state = controller?.state ?? null;
  const pendingFocus = useRef<string | null>(null);
  const tabRefs = useRef(new Map<string, HTMLAnchorElement>());

  // After closing the active tab, keyboard focus lands on the newly selected
  // neighbour; the selected tab stays scrolled into view. Inactive closes
  // never steal focus.
  useLayoutEffect(() => {
    if (state) tabRefs.current.get(state.active)?.scrollIntoView({ block: "nearest", inline: "nearest" });
    const target = pendingFocus.current;
    if (target === null) return;
    pendingFocus.current = null;
    tabRefs.current.get(target)?.focus();
  }, [state]);

  if (!controller || !state) return null;

  function requestClose(id: string) {
    if (!state) return;
    const index = state.tabs.findIndex(tab => tab.id === id);
    if (index === -1) return;
    if (state.active === id) {
      const neighbour = state.tabs[index + 1] ?? state.tabs[index - 1];
      pendingFocus.current = neighbour?.id ?? null;
    }
    controller!.close(id);
  }

  function glyphFor(href: string): ReactNode {
    const path = href.split(/[?#]/, 1)[0];
    const match = /^\/app\/conversations\/([^/]+)/.exec(path);
    const summary = match ? conversations?.find((item: Conversation) => match[1] === encodeURIComponent(item.id)) : undefined;
    const running = !!summary && (summary.status === "queued" || summary.status === "running");
    return tabGlyph(path, running);
  }

  return <div className={styles.sessionTabs} data-session-tabs role="navigation" aria-label="Workspace tabs">
    <div className={styles.sessionTabsScroll}>
      {state.tabs.map(tab => {
        const entry = tab.entries[tab.index] ?? tab.entries[0];
        const active = tab.id === state.active;
        return <span key={tab.id} className={styles.sessionTabItem} data-active={active || undefined}>
          <a ref={element => { if (element) tabRefs.current.set(tab.id, element); else tabRefs.current.delete(tab.id); }} href={entry.href} className={styles.sessionTabLink} title={entry.title} aria-current={active ? "page" : undefined}
            onClick={event => {
              if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
              event.preventDefault();
              controller!.select(tab.id);
            }}
            onAuxClick={event => {
              if (event.button !== 1) return;
              event.preventDefault();
              requestClose(tab.id);
            }}
          ><span className={styles.sessionTabMark} aria-hidden="true">{glyphFor(entry.href)}</span><span className={styles.sessionTabTitle}>{entry.title}</span></a>
          <button type="button" className={styles.sessionTabClose} aria-label={`Close ${entry.title} tab`} onClick={() => requestClose(tab.id)}><WorkspaceIcon name="close" width="12" height="12" /></button>
        </span>;
      })}
    </div>
    <button type="button" className={styles.sessionTabNew} aria-label="New tab" title="New tab" onClick={() => controller!.openTab()}><WorkspaceIcon name="plus" width="14" height="14" /></button>
  </div>;
}

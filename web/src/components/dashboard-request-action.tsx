"use client";

import { useId, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useDialog } from "@/components/use-dialog";
import { usePopover } from "@/components/use-popover";
import { useWorkspace } from "@/components/workspace-context";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { WrenAvatar } from "@/components/wren-avatar";
import { createDashboardRequest, storeDashboardRequest } from "@/lib/dashboard-requests";
import { dashboardRequestDestinations, flattenDashboardRequestDestinations, newCanterDestinationKey, type DashboardRequestDestination } from "@/lib/dashboard-request-destinations";
import type { Conversation, OperatorSurface } from "@/lib/operator-api";
import { useWrenResource, workBase } from "@/lib/wren-work";
import { conversationHref } from "@/lib/wren-presentation";
import { workspaceDisplayName } from "@/lib/workspace-selection";
import type { Wren, WrenRoom } from "@/lib/wrens";
import styles from "@/components/dashboard-request-action.module.css";
import sharedStyles from "@/components/workspace.module.css";

export type DashboardRequestSource = { accountId: string; workspaceId: string; workspaceName: string };

export function DashboardRequestAction({ prompt, surface, label, disabled, className }: { prompt: string; surface?: OperatorSurface; label?: string; disabled?: boolean; className?: string }) {
  const { data } = useWorkspace();
  const [openScope, setOpenScope] = useState<DashboardRequestSource | null>(null);
  if (!data) return null;
  // Viewers inspect everything but never originate mutation requests.
  if (data.workspace.role === "viewer") return null;
  const currentScope = { accountId: data.account.id, workspaceId: data.workspace.id, workspaceName: workspaceDisplayName(data.workspace.name) };
  // The default "Ask Canter" control is plain navigation to the main
  // dashboard — only explicitly labelled request actions open the takeover.
  if (!label || label === "Ask Canter") return <Link href="/app" className={className ?? sharedStyles.primaryButton}>Ask Canter</Link>;
  return <>
    <button type="button" className={className ?? sharedStyles.primaryButton} disabled={disabled} onClick={() => setOpenScope(currentScope)}>{label}</button>
    {openScope ? <DashboardRequestDialog source={openScope} prompt={prompt} surface={surface} onClose={() => setOpenScope(null)} /> : null}
  </>;
}

// Parent-owned variant: hosts that keep the takeover outside their own menus
// mount this directly so a closing popover cannot unmount the request.
export function DashboardRequestDialog({ source, prompt, surface, onClose }: { source: DashboardRequestSource; prompt: string; surface?: OperatorSurface; onClose: () => void }) {
  const { data } = useWorkspace();
  // A viewer never originates requests — if the role flips while the takeover
  // is open it closes rather than lingering for the receiver to reject.
  if (data?.workspace.role === "viewer") return null;
  const current = data ? { accountId: data.account.id, workspaceId: data.workspace.id } : null;
  return <RequestTakeover key={`${source.accountId}:${source.workspaceId}`} source={source} current={current} accountId={source.accountId} workspaceId={source.workspaceId} conversations={data?.conversations ?? []} prompt={prompt} surface={surface} onClose={onClose} />;
}

const surfaceLabel = (surface?: OperatorSurface) => surface?.kind === "super-sandbox" && surface.id ? `Super Sandbox ${surface.id}` : surface?.kind === "super-sandboxes" ? "Super Sandboxes" : surface?.kind === "sandbox" && surface.id ? `Sandbox ${surface.id}` : surface?.kind === "sandboxes" ? "Sandboxes" : surface?.kind === "apps" ? "Apps" : null;

function RequestTakeover({ source, current, accountId, workspaceId, conversations, prompt, surface, onClose }: { source: DashboardRequestSource; current: { accountId: string; workspaceId: string } | null; accountId: string; workspaceId: string; conversations: Conversation[]; prompt: string; surface?: OperatorSurface; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const dismiss = useDialog(ref, "textarea");
  const router = useRouter();
  const composerBox = useRef<HTMLDivElement>(null);
  const selectorRoot = useRef<HTMLDivElement>(null);
  const selectorTrigger = useRef<HTMLButtonElement>(null);
  const selectorId = useId();
  const [text, setText] = useState(prompt);
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const [selectorOpen, setSelectorOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedKey, setSelectedKey] = useState(newCanterDestinationKey);
  usePopover(selectorOpen, selectorRoot, () => setSelectorOpen(false));
  const wrens = useWrenResource<{ wrens: Wren[] }>(`${workBase(workspaceId)}/wrens`);
  const rooms = useWrenResource<{ rooms: WrenRoom[] }>(`${workBase(workspaceId)}/rooms`);

  const groups = useMemo(() => dashboardRequestDestinations({ conversations, wrens: wrens.data?.wrens ?? [], rooms: rooms.data?.rooms ?? [] }, conversationHref), [conversations, wrens.data, rooms.data]);
  const flat = useMemo(() => flattenDashboardRequestDestinations(groups), [groups]);
  const selected = flat.find(destination => destination.key === selectedKey) ?? null;
  const needle = search.trim().toLowerCase();
  const matches = (destination: DashboardRequestDestination) => !needle || destination.label.toLowerCase().includes(needle) || destination.detail.toLowerCase().includes(needle);

  // The takeover keeps the scope it opened under: if the active organisation
  // changes while it is open the request must become inert, never re-bind.
  const staleScope = !current || current.accountId !== source.accountId || current.workspaceId !== source.workspaceId;
  const inert = pending || staleScope;
  const listError = [wrens.error, rooms.error].filter(Boolean).join(" ");

  function choose(destination: DashboardRequestDestination) {
    // Picking a destination never navigates — only Continue does.
    if (inert) return;
    setSelectedKey(destination.key);
    setSelectorOpen(false);
    setSearch("");
    // The chosen option unmounts with the popup — hand focus back to the
    // stable trigger so keyboard users keep their place.
    selectorTrigger.current?.focus({ preventScroll: true });
  }

  function continueRequest() {
    if (inert || !text.trim() || !selected) return;
    const box = composerBox.current?.getBoundingClientRect();
    const composerOrigin = box && box.width > 0 ? { x: box.left, y: box.top, width: box.width, height: box.height, viewportWidth: window.innerWidth, viewportHeight: window.innerHeight } : undefined;
    const request = createDashboardRequest({ accountId, workspaceId, conversationId: selected.conversationId, prompt: text, surface, composerOrigin });
    if (!request) { setError("This request could not be prepared. Check the text and try again."); return; }
    try {
      storeDashboardRequest(request, window.sessionStorage);
    } catch (cause) {
      setError(cause instanceof Error ? `Couldn't keep this request: ${cause.message}` : "Couldn't keep this request.");
      return;
    }
    setPending(true);
    setError("");
    const join = selected.href.includes("?") ? "&" : "?";
    router.push(`${selected.href}${join}dashboardRequest=${encodeURIComponent(request.nonce)}&workspaceId=${encodeURIComponent(workspaceId)}`);
  }

  const destinationButton = (destination: DashboardRequestDestination) => (
    <button key={destination.key} type="button" className={styles.selectorItem} disabled={inert} onClick={() => choose(destination)}>
      {destination.group === "wren" && destination.wren ? <span className={styles.itemMark}><WrenAvatar shape={destination.wren.shape} palette={destination.wren.palette} size={18} /></span> : null}
      {destination.group === "room" ? <span className={styles.itemMark}><WorkspaceIcon name="message" width="14" height="14" /></span> : null}
      {destination.group === "canter" ? <span className={styles.itemMark}><WorkspaceIcon name={destination.key === newCanterDestinationKey ? "plus" : "reply"} width="14" height="14" /></span> : null}
      <span className={styles.itemText} title={destination.label}>{destination.label}</span>
      {destination.key === selectedKey ? <span className={styles.itemCheck}><WorkspaceIcon name="check" width="14" height="14" /></span> : <span className={styles.itemDetail}>{destination.detail}</span>}
    </button>
  );

  const destinationGroup = (title: string, items: DashboardRequestDestination[]) => {
    const visible = items.filter(matches);
    if (!visible.length) return null;
    return <div key={title} role="group" aria-label={title}>
      <p className={styles.selectorGroup}>{title}</p>
      {visible.map(destinationButton)}
    </div>;
  };

  const selectorEmpty = ![...groups.canter, ...groups.wrens, ...groups.rooms].some(matches);

  return <dialog ref={ref} className={styles.dialog} aria-labelledby="dashboard-request-title" onCancel={event => { event.preventDefault(); if (selectorOpen) { setSelectorOpen(false); setSearch(""); return; } dismiss(onClose); }}>
    <button type="button" className={styles.close} aria-label="Close" onClick={() => dismiss(onClose)}><WorkspaceIcon name="close" width="17" height="17" /></button>
    <div className={styles.stage}>
      <span className={`wordmark ${styles.brand}`}>canter</span>
      <h1 id="dashboard-request-title" className={styles.heading}>Ask Canter</h1>
      <p className={styles.context}>Organisation: {source.workspaceName || "another organisation"}{surfaceLabel(surface) ? ` · ${surfaceLabel(surface)}` : ""}</p>

      <div className={styles.composer} ref={composerBox} data-dashboard-request-composer>
        <textarea className={styles.request} aria-label="Request" value={text} onChange={event => setText(event.target.value)} rows={4} maxLength={8000} disabled={inert} />
        {surface ? <span className={styles.contextChip}><WorkspaceIcon name="terminal" width="12" height="12" />{surfaceLabel(surface)}</span> : null}
        <div className={styles.composerBar}>
          <div ref={selectorRoot} className={styles.selector}>
            <button type="button" ref={selectorTrigger} className={styles.selectorButton} aria-expanded={selectorOpen} aria-controls={selectorOpen ? selectorId : undefined} aria-label={`Conversation: ${selected?.label ?? "Choose a conversation"}`} disabled={inert} onClick={() => { setSelectorOpen(open => !open); setSearch(""); }}>
              <span className={styles.selectedLabel}>{selected ? selected.label : "Choose a conversation"}</span>
              <WorkspaceIcon name="down" width="13" height="13" />
            </button>
            {selectorOpen ? <div className={styles.selectorPop} id={selectorId}>
              <input className={styles.selectorSearch} aria-label="Search conversations" placeholder="Search conversations" value={search} onChange={event => setSearch(event.target.value)} disabled={inert} autoFocus />
              <div className={styles.selectorList} role="group" aria-label="Destinations">
                {destinationGroup("Canter", groups.canter)}
                {destinationGroup("Your Wrens", groups.wrens)}
                {destinationGroup("Rooms", groups.rooms)}
                {selectorEmpty ? <p className={styles.selectorEmpty}>No matching conversations.</p> : null}
              </div>
            </div> : null}
          </div>
          <button type="button" className={styles.continue} disabled={inert || !text.trim() || !selected} onClick={continueRequest}>Continue <WorkspaceIcon name="right" width="14" height="14" /></button>
        </div>
      </div>

      <p className={styles.footerNote}>You&rsquo;ll review and send this in the conversation.</p>
      {staleScope ? <p role="alert" className={styles.alert}>This request was drafted for {source.workspaceName || "another organisation"}. Close it and start again from the dashboard you want.</p> : null}
      {!selected && !staleScope ? <p role="alert" className={styles.alert}>That destination is no longer available — choose another conversation.</p> : null}
      {listError ? <p role="status" className={styles.note}>Some conversations couldn&rsquo;t be loaded — Canter is still available. {listError}</p> : null}
      {error ? <p className={styles.alert} role="alert">{error}</p> : null}
    </div>
  </dialog>;
}

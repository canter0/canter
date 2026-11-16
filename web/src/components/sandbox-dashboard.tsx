"use client";

import Link from "next/link";
import { useMemo, useRef, useState } from "react";
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from "@ui-sdk/react/menu";
import { Meter, MeterIndicator, MeterTrack } from "@ui-sdk/react/meter";
import { SegmentedControl, SegmentedControlIndicator, SegmentedControlItem } from "@ui-sdk/react/segmented-control";
import { Tooltip, TooltipContent, TooltipTrigger } from "@ui-sdk/react/tooltip";
import { DashboardRequestDialog, type DashboardRequestSource } from "@/components/dashboard-request-action";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { relativeTime } from "@/lib/canter-api";
import { sandboxRequestPrompt, type SandboxRequestAction } from "@/lib/dashboard-request-prompts";
import type { OperatorSurface } from "@/lib/operator-api";
import { sandboxHourlyCents, sandboxPhaseText, type Sandbox, type SandboxListResponse } from "@/lib/sandboxes";
import { useWorkspace } from "@/components/workspace-context";
import { workspaceDisplayName } from "@/lib/workspace-selection";
import { useWrenResource, workBase } from "@/lib/wren-work";
import { SandboxSearch, SandboxSelect } from "./sandbox-controls";
import shared from "@/components/workspace.module.css";
import styles from "@/components/sandbox-dashboard.module.css";

// Conversation-first dashboards must keep reflecting agent-side changes, so
// the list polls on every visible interval — never just while work is active.
const alwaysLive = () => true;

type Group = "running" | "asleep" | "attention" | "changing" | "deleted";
type Filter = "all" | Group;
type Sort = "activity" | "name" | "cost";

const groupOf = (phase: string): Group => phase === "ready" ? "running"
  : phase === "asleep" ? "asleep"
  : phase === "failed" || phase === "save_failed" ? "attention"
  : phase === "deleted" || phase === "deleting" ? "deleted"
  : "changing";

const filters: { value: Filter; label: string }[] = [
  { value: "all", label: "All" },
  { value: "running", label: "Running" },
  { value: "asleep", label: "Asleep" },
  { value: "changing", label: "Changing" },
  { value: "attention", label: "Attention" },
  { value: "deleted", label: "Deleted" },
];

const sorts: { value: Sort; label: string }[] = [
  { value: "activity", label: "Recently active" },
  { value: "name", label: "Name" },
  { value: "cost", label: "Hourly rate" },
];

const activityTime = (sandbox: Sandbox) => Date.parse(sandbox.lastUsedAt ?? sandbox.createdAt) || 0;
const lastActive = (value?: string) => {
  if (!value) return "Not used yet";
  const time = relativeTime(value);
  return time === "now" ? "Just now" : `${time.replace(/^1 days$/, "1 day")} ago`;
};
const hourly = (sandbox: Sandbox) => sandbox.tier === "paid" ? sandbox.monthlyCents / 720 : 0;

function requestItems(sandbox: Sandbox): { action: SandboxRequestAction; label: string }[] {
  if (["deleting", "deleted"].includes(sandbox.phase)) return [];
  const ready = sandbox.phase === "ready", asleep = sandbox.phase === "asleep", saveFailed = sandbox.phase === "save_failed";
  return [
    ...(ready ? [{ action: "command" as const, label: "Ask to run a command" }] : []),
    ...(ready || saveFailed ? [{ action: "sleep" as const, label: saveFailed ? "Request retry sleep" : "Request sleep" }] : []),
    ...(asleep || saveFailed ? [{ action: "wake" as const, label: asleep ? "Request wake" : "Request resume" }] : []),
    { action: "save", label: "Request a save" },
    { action: "settings", label: "Request settings change" },
    { action: "delete", label: "Request delete" },
  ];
}

export function SandboxDashboard({ workspaceId, onSelect, onRequest }: { workspaceId: string; onSelect?: (surface: OperatorSurface) => void; onRequest?: (prompt: string) => void }) {
  const { data } = useWorkspace();
  const scoped = data?.workspace.id === workspaceId;
  const writable = scoped && (data?.workspace.role === "owner" || data?.workspace.role === "operator");
  const list = useWrenResource<SandboxListResponse>(`${workBase(workspaceId)}/sandboxes`, 5000, alwaysLive);
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [sort, setSort] = useState<Sort>("activity");
  const [dialog, setDialog] = useState<{ source: DashboardRequestSource; prompt: string; id: string } | null>(null);
  const result = list.data;
  const sandboxes = useMemo(() => result?.sandboxes ?? [], [result]);

  const counts = useMemo(() => {
    const byGroup: Record<Filter, number> = { all: sandboxes.length, running: 0, asleep: 0, attention: 0, changing: 0, deleted: 0 };
    for (const sandbox of sandboxes) byGroup[groupOf(sandbox.phase)]++;
    return byGroup;
  }, [sandboxes]);
  const runningRate = sandboxes.filter(sandbox => sandbox.phase === "ready").reduce((sum, sandbox) => sum + hourly(sandbox), 0);
  const shownFilters = filters.filter(item => item.value === "all" || item.value === filter || counts[item.value] > 0);

  const needle = search.trim().toLowerCase();
  const visible = sandboxes
    .filter(sandbox => filter === "all" || groupOf(sandbox.phase) === filter)
    .filter(sandbox => !needle || [sandbox.name, sandbox.id, sandbox.size, sandbox.createdByName ?? ""].some(value => value.toLowerCase().includes(needle)))
    .sort((a, b) => sort === "name" ? a.name.localeCompare(b.name) : sort === "cost" ? hourly(b) - hourly(a) : activityTime(b) - activityTime(a));

  function ask(sandbox: Sandbox, action: SandboxRequestAction) {
    if (!writable || !data) return;
    const prompt = sandboxRequestPrompt(action, sandbox);
    if (onRequest) onRequest(prompt);
    else setDialog({ source: { accountId: data.account.id, workspaceId, workspaceName: workspaceDisplayName(data.workspace.name) }, prompt, id: sandbox.id });
  }

  const freeUsed = result ? Math.floor(result.freeUsage.usedSeconds / 3600) : 0;
  const freeLimit = result ? Math.max(1, Math.floor(result.freeUsage.limitSeconds / 3600)) : 1;

  return <section className={styles.root} aria-label="Sandboxes dashboard">
    <header className={shared.pageHeading} data-motion="reveal">
      <div><h1>Sandboxes</h1>{result ? <p>{counts.all} {counts.all === 1 ? "computer" : "computers"}{counts.running ? ` · ${counts.running} running` : ""}</p> : null}</div>
      {writable && result?.enabled ? <Link href="/app" className={shared.primaryButton}><WorkspaceIcon name="plus" width="16" height="16" />Ask Canter</Link> : null}
    </header>

    {list.error ? <div role="alert" className={shared.error}>{result ? "Couldn’t refresh sandboxes. Showing the last loaded list. " : ""}{list.error} <button onClick={list.refresh}>Try again</button></div> : null}
    {result && !result.enabled ? <p role="status" className={styles.notice}>Sandboxes are temporarily unavailable. Existing resources are shown below.</p> : null}

    {!result ? (list.error ? null : <div className={styles.skeleton} role="status" aria-label="Loading sandboxes"><span /><span /><span /></div>) : <>
      <dl className={styles.stats}>
        <div><dt>Running</dt><dd><span className={styles.dot} data-tone="running" />{counts.running}</dd></div>
        <div><dt>Asleep</dt><dd><span className={styles.dot} data-tone="asleep" />{counts.asleep}</dd></div>
        <div><dt>Attention</dt><dd><span className={styles.dot} data-tone={counts.attention ? "attention" : "asleep"} />{counts.attention}</dd></div>
        <div><dt>Running rate</dt><dd className={styles.mono}>{runningRate ? `$${(runningRate / 100).toFixed(3)}/h` : "$0/h"}</dd></div>
        <div className={styles.usage}>
          <dt>Free hours</dt>
          <dd>
            <span className={styles.mono}>{freeUsed}<small> / {freeLimit} h</small></span>
            <Meter value={Math.min(freeUsed, freeLimit)} max={freeLimit} aria-label={`${freeUsed} of ${freeLimit} free hours used this month`} className={styles.meter}>
              <MeterTrack className={styles.meterTrack}><MeterIndicator className={styles.meterIndicator} /></MeterTrack>
            </Meter>
          </dd>
        </div>
      </dl>

      {sandboxes.length ? <>
        <div className={styles.toolbar}>
          <SegmentedControl value={filter} onValueChange={value => { if (value) setFilter(value); }} aria-label="Filter by state" className={styles.segments}>
            <SegmentedControlIndicator className={styles.segmentIndicator} />
            {shownFilters.map(item => <SegmentedControlItem key={item.value} value={item.value} className={styles.segment}>
              {item.label}<span className={styles.count}>{counts[item.value]}</span>
            </SegmentedControlItem>)}
          </SegmentedControl>
          <SandboxSearch label="Search sandboxes" placeholder="Search name, size, creator" value={search} onChange={setSearch} shortcut={onSelect ? undefined : "/"} />
          <SandboxSelect label="Sort sandboxes" value={sort} onChange={value => setSort(value as Sort)} options={sorts} className={styles.sort} />
        </div>

        {visible.length ? <div className={styles.listWrap}>
          <div className={styles.listHead} aria-hidden="true"><span /><span>Name</span><span>State</span><span>Machine</span><span>Last activity</span><span>Rate</span><span /></div>
          <ul className={styles.list}>{visible.map(sandbox => <SandboxRow
            key={sandbox.id}
            sandbox={sandbox}
            writable={writable}
            onSelect={onSelect}
            onAsk={action => ask(sandbox, action)}
          />)}</ul>
          <p className={styles.footer}>{visible.length === sandboxes.length ? `${visible.length} shown` : `${visible.length} of ${sandboxes.length} shown`}</p>
        </div> : <div className={styles.empty} data-motion="reveal">
          <p>No matching sandboxes</p>
          <button type="button" className={shared.secondaryButton} onClick={() => { setSearch(""); setFilter("all"); }}>Clear filters</button>
        </div>}
      </> : <div className={styles.empty} data-motion="reveal">
        <WorkspaceIcon name="desktop" width="22" height="22" />
        <p>No sandboxes yet</p>
      </div>}
    </>}
    {dialog && !onRequest ? <DashboardRequestDialog source={dialog.source} prompt={dialog.prompt} surface={{ kind: "sandbox", id: dialog.id }} onClose={() => setDialog(null)} /> : null}
  </section>;
}

function SandboxRow({ sandbox, writable, onSelect, onAsk }: { sandbox: Sandbox; writable: boolean; onSelect?: (surface: OperatorSurface) => void; onAsk: (action: SandboxRequestAction) => void }) {
  const group = groupOf(sandbox.phase);
  const items = writable ? requestItems(sandbox) : [];
  const pending = useRef<SandboxRequestAction | null>(null);
  const href = `/app/sandboxes/${encodeURIComponent(sandbox.id)}`;
  const open = onSelect
    ? <button type="button" className={styles.open} onClick={() => onSelect({ kind: "sandbox", id: sandbox.id })}>{sandbox.name}</button>
    : <Link href={href} className={styles.open}>{sandbox.name}</Link>;

  const row = <>
    <Tooltip>
      <TooltipTrigger render={<span className={styles.dotCell} tabIndex={-1} />} aria-label={sandboxPhaseText(sandbox.phase)}>
        <span className={styles.dot} data-tone={group} />
      </TooltipTrigger>
      <TooltipContent side="top" sideOffset={6} className={`dashboard-theme ${styles.tip}`}>{sandbox.failure ?? sandboxPhaseText(sandbox.phase)}</TooltipContent>
    </Tooltip>
    <div className={styles.identity}>
      {open}
      <code>{sandbox.id}</code>
    </div>
    <span className={styles.state} data-tone={group}>{sandboxPhaseText(sandbox.phase)}</span>
    <span className={styles.machine}>
      <span>{sandbox.size}</span>
      {sandbox.desktop ? <span className={styles.chip}>Desktop</span> : null}
      {sandbox.hasCheckpoint || sandbox.snapshotId ? <span className={styles.chip}>Saved</span> : null}
    </span>
    <span className={styles.activity}>{lastActive(sandbox.lastUsedAt)}{sandbox.createdByName ? <small>by {sandbox.createdByName}</small> : null}</span>
    <span className={styles.rate}>{sandbox.tier === "paid" ? sandboxHourlyCents(sandbox.monthlyCents) : "Free tier"}</span>
  </>;

  if (!items.length) return <li className={styles.row} data-tone={group}>{row}<span /></li>;

  return <li className={styles.row} data-tone={group}>{row}<Menu onOpenChangeComplete={isOpen => {
    // Run the chosen request only after the menu has finished closing, so
    // the request dialog never races the menu's focus recovery.
    if (isOpen || !pending.current) return;
    const action = pending.current;
    pending.current = null;
    onAsk(action);
  }}>
    <MenuTrigger className={styles.rowMenu} aria-label={`Requests for ${sandbox.name}`}><WorkspaceIcon name="more" width="16" height="16" /></MenuTrigger>
    <MenuContent className={`dashboard-theme ${styles.menu}`} side="bottom" align="end" sideOffset={4}>
      {onSelect
        ? <MenuItem className={styles.menuItem} label="Open" onSelect={() => onSelect({ kind: "sandbox", id: sandbox.id })}>Open</MenuItem>
        : <MenuItem className={styles.menuItem} label="Open" render={<Link href={href} />}>Open</MenuItem>}
      <MenuSeparator className={styles.separator} />
      {items.map(item => <MenuItem
        key={item.action}
        className={styles.menuItem}
        label={item.label}
        destructive={item.action === "delete"}
        onSelect={() => { pending.current = item.action; }}
      >{item.action === "delete" ? <span className={shared.status} data-phase="failed">{item.label}</span> : item.label}</MenuItem>)}
    </MenuContent>
  </Menu></li>;
}

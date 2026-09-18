"use client";

import { useState } from "react";
import { dollars } from "@/lib/pricing";
import styles from "@/app/app/billing/billing.module.css";

export type BillingUsage = {
  start: string; end: string; asOf: string; recordedCents: number; eventCount: number; lastRecordedAt: string | null;
  days: { date: string; cents: number; events: number }[];
  resources: { resource: string; cents: number; recentCents: number; previousCents: number }[];
  capacity: { name: string; hosts: number; memoryMiB: number; vcpu: number; instances: number }[];
  forecast: { usageCents: number; totalCents: number; dailyCents: number; sampleDays: number; remainingDays: number } | null;
  trendReady: boolean; recentCents: number; previousCents: number;
};

const dayLabel = (value: string) => new Date(value + "T12:00:00Z").toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
const memory = (mib: number) => `${Number((mib / 1024).toFixed(1))} GB`;
const amount = (cents: number) => (cents / 100).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });

export function BillingUsageView({ usage }: { usage: BillingUsage }) {
  const [hovered, setHovered] = useState<string | null>(null);
  const [range, setRange] = useState("period");
  const cutoff = new Date(usage.asOf);
  cutoff.setUTCDate(cutoff.getUTCDate() - (range === "7" ? 6 : 29));
  const days = range === "period" ? usage.days : usage.days.filter(day => day.date >= cutoff.toISOString().slice(0, 10));
  const records = days.reduce((sum, day) => sum + day.events, 0);
  const maximum = Math.max(...days.map(day => day.cents), 1);
  const magnitude = 10 ** Math.floor(Math.log10(maximum));
  const chartMaximum = Math.ceil(maximum / magnitude) * magnitude;
  const selected = days.find(day => day.date === hovered);
  const selectedIndex = selected ? days.indexOf(selected) : -1;
  const axisDays = [...new Set([0, Math.floor((days.length - 1) / 3), Math.floor((days.length - 1) * 2 / 3), days.length - 1])].filter(index => index >= 0);
  const capacity = usage.capacity.reduce((sum, item) => ({ hosts: sum.hosts + item.hosts, memory: sum.memory + item.memoryMiB, vcpu: sum.vcpu + item.vcpu, instances: sum.instances + item.instances }), { hosts: 0, memory: 0, vcpu: 0, instances: 0 });
  const dateRange = days.length ? `${dayLabel(days[0].date)} – ${dayLabel(days[days.length - 1].date)}` : "This billing period";

  function exportUsage() {
    const csv = ["Date (UTC),Recorded usage (USD),Usage records", ...days.map(day => `${day.date},${(day.cents / 100).toFixed(2)},${day.events}`)].join("\r\n");
    const url = URL.createObjectURL(new Blob([csv], { type: "text/csv;charset=utf-8;" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `canter-usage-${days[0]?.date ?? "period"}-${days.at(-1)?.date ?? "current"}.csv`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  return <>
    {usage.eventCount > 0 ? <>
    <div className={styles.toolbar}>
      <div className={styles.filters}>
        <label className={styles.filter}><select aria-label="Usage date range" value={range} onChange={event => { setRange(event.target.value); setHovered(null); }}><option value="period">This billing period</option><option value="7">Last 7 days</option><option value="30">Last 30 days</option></select></label>
        {range !== "period" ? <button className={styles.clearFilter} onClick={() => { setRange("period"); setHovered(null); }}>Clear filters</button> : null}
      </div>
      <button className={styles.secondaryButton} onClick={exportUsage} disabled={!records}><svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M8 2v8m-3-3 3 3 3-3M3 11v3h10v-3" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" /></svg>Export</button>
    </div>
    <section className={styles.chartCard} aria-label="Daily usage chart">
      <header><h2>Daily spending</h2><span className={styles.legend}>USD</span></header>
      <div className={styles.chartBody}>
        <div className={styles.chartGrid} aria-hidden="true">{[1, 0.5, 0].map(tick => <div key={tick}><span>{usage.eventCount ? (chartMaximum * tick / 100).toLocaleString("en-US", { maximumFractionDigits: 3 }) : tick === 0 ? "0" : ""}</span><i /></div>)}</div>
        {records ? <div className={styles.chart} role="group" aria-label="Daily recorded spend" onMouseLeave={() => setHovered(null)}>
          {days.map(day => <button key={day.date} aria-label={`${dayLabel(day.date)}: ${dollars(day.cents)} recorded usage, ${day.events} records`} onMouseEnter={() => setHovered(day.date)} onFocus={() => setHovered(day.date)} onBlur={() => setHovered(null)} onClick={() => setHovered(day.date)} data-selected={selected?.date === day.date}><span style={{ height: day.cents ? `${Math.max(0.8, day.cents / chartMaximum * 100)}%` : "0" }} /></button>)}
          {selected ? <div className={styles.chartTooltip} style={{ left: `${Math.max(15, Math.min(85, (selectedIndex + 0.5) / days.length * 100))}%` }}><span>{dayLabel(selected.date)}</span><strong>${amount(selected.cents)}</strong><small>{selected.events.toLocaleString()} usage records</small></div> : null}
        </div> : <div className={styles.emptyChart}><p>No recorded usage in this range.</p></div>}
      </div>
      <div className={styles.chartAxis}>{axisDays.map(index => <span key={index}>{dayLabel(days[index].date)}</span>)}</div>
    </section>
    <p className={styles.chartNote}>{dateRange} · UTC</p>
    </> : null}
    <section className={styles.usageSection} aria-label="Resource usage">
      <header><h2>Resources</h2><span>{usage.capacity.length} app{usage.capacity.length === 1 ? "" : "s"} · Current period</span></header>
      <div className={styles.resourceCard}>
        <div className={styles.capacityMetrics}><div><strong>{capacity.hosts}</strong><span>Configured hosts</span></div><div><strong>{capacity.instances}</strong><span>Service instances</span></div><div><strong>{capacity.vcpu}</strong><span>Allocated vCPU</span></div><div><strong>{memory(capacity.memory)}</strong><span>Host memory</span></div></div>
        {usage.resources.length ? <div className={styles.resourceList}>{usage.resources.map(item => <div key={item.resource}><span>{item.resource}<small>{usage.trendReady ? (item.recentCents > item.previousCents ? "Spending increased this week" : item.recentCents < item.previousCents ? "Spending decreased this week" : "No change this week") : "Recorded this period"}</small></span><strong>{dollars(item.cents)}</strong></div>)}</div> : usage.capacity.length ? <div className={styles.resourceList}>{usage.capacity.map(item => <div key={item.name}><span>{item.name}<small>{item.hosts} host{item.hosts === 1 ? "" : "s"} · {memory(item.memoryMiB)} memory · {item.instances} service instance{item.instances === 1 ? "" : "s"}</small></span><small>Unmetered</small></div>)}</div> : <p className={styles.small}>No apps are configured in this workspace yet.</p>}
      </div>
      <p className={styles.chartNote}>Capacity reflects your app configuration, not measured utilization.</p>
    </section>

  </>;
}

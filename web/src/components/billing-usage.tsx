"use client";
import { useState } from "react";
import { dollars, estimateBill, type PlanID } from "@/lib/pricing";
import styles from "@/app/app/billing/billing.module.css";
export type BillingUsage = {
 start: string; end: string; asOf: string; recordedCents: number; eventCount: number; lastRecordedAt: string | null;
 days: {date:string;cents:number;events:number}[];
 resources: {resource:string;cents:number;recentCents:number;previousCents:number}[];
 capacity: {name:string;hosts:number;memoryMiB:number;vcpu:number;instances:number}[];
 forecast: {usageCents:number;totalCents:number;dailyCents:number;sampleDays:number;remainingDays:number}|null;
 trendReady:boolean;recentCents:number;previousCents:number;
};
const dayLabel=(value:string)=>new Date(value+'T12:00:00Z').toLocaleDateString(undefined,{month:'short',day:'numeric',timeZone:'UTC'});
const memory=(mib:number)=>`${Number((mib/1024).toFixed(1))} GB`;
export function BillingUsageView({usage,plan}: {usage:BillingUsage;plan:PlanID}) {
 const [hovered,setHovered]=useState<string|null>(null);
 const [change,setChange]=useState(0);
 const days=usage.days.slice(-30);
 const maximum=Math.max(...days.map(day=>day.cents),1);
 const selected=days.find(day=>day.date===hovered)??days.at(-1);
 const capacity=usage.capacity.reduce((sum,item)=>({hosts:sum.hosts+item.hosts,memory:sum.memory+item.memoryMiB,vcpu:sum.vcpu+item.vcpu,instances:sum.instances+item.instances}),{hosts:0,memory:0,vcpu:0,instances:0});
 const forecast=usage.forecast;
 const projected=forecast?estimateBill(plan,Math.min(1_000_000_000,Math.round(usage.recordedCents+forecast.dailyCents*forecast.remainingDays*(1+change/100)))).totalCents:null;
 const trend=usage.recentCents-usage.previousCents;
 const dateRange=`${new Date(usage.start).toLocaleDateString(undefined,{month:'short',day:'numeric',timeZone:'UTC'})} – ${new Date(usage.end).toLocaleDateString(undefined,{month:'short',day:'numeric',timeZone:'UTC'})}`;
 return <>
  <p className={styles.period}>{dateRange}<span>USD · before tax</span></p>
  <section className={styles.metrics} aria-label="Usage summary">
   <div><span>Recorded usage</span><strong>{usage.eventCount?dollars(usage.recordedCents):'—'}</strong><small>{usage.eventCount?`${usage.eventCount.toLocaleString()} usage records`:'No usage records yet'}</small></div>
   <div><span>Projected period total</span><strong>{forecast?dollars(forecast.totalCents):'—'}</strong><small>{forecast?`At the last ${forecast.sampleDays} days’ pace`:'Needs at least 3 days of recorded usage'}</small></div>
   <div><span>Spending trend</span><strong className={trend>0?styles.increase:trend<0?styles.decrease:undefined}>{usage.trendReady?`${trend>0?'+':trend<0?'−':''}${dollars(Math.abs(trend))}`:'—'}</strong><small>{usage.trendReady?'Last 7 days vs. previous 7':'Waiting for two weeks of history'}</small></div>
  </section>
  <section className={styles.usageSection} aria-label="Daily usage chart"><header><h2>Usage over time</h2><span>Last {days.length || 30} days · UTC</span></header>
   {usage.eventCount ? <><div className={styles.chart} role="group" aria-label="Daily recorded spend">{days.map(day=><button key={day.date} aria-label={`${dayLabel(day.date)}: ${dollars(day.cents)} recorded usage`} onMouseEnter={()=>setHovered(day.date)} onFocus={()=>setHovered(day.date)} onClick={()=>setHovered(day.date)} data-selected={selected?.date===day.date}><span style={{height:`${Math.max(2,day.cents/maximum*100)}%`}} /></button>)}</div><div className={styles.chartAxis}><span>{days[0]?dayLabel(days[0].date):''}</span><span>{selected?`${dayLabel(selected.date)} · ${dollars(selected.cents)}`:''}</span><span>{days.at(-1)?dayLabel(days.at(-1)!.date):''}</span></div></> : <div className={styles.emptyChart}><span className={styles.emptyChartIcon}>↗</span><h3>Your usage will appear here</h3><p>Once metered usage arrives, you’ll see daily spending, trends, and a period forecast. Configured resources are shown below.</p></div>}
   {usage.lastRecordedAt?<p className={styles.small}>Latest usage: {new Date(usage.lastRecordedAt).toLocaleString()}. Records may arrive late.</p>:null}
  </section>
  <section className={styles.usageSection}><header><h2>What’s using resources</h2><span>{usage.capacity.length} app{usage.capacity.length===1?'':'s'}</span></header>
   <div className={styles.capacityMetrics}><div><strong>{capacity.hosts}</strong><span>Configured hosts</span></div><div><strong>{capacity.instances}</strong><span>Service instances</span></div><div><strong>{capacity.vcpu}</strong><span>Allocated vCPU</span></div><div><strong>{memory(capacity.memory)}</strong><span>Host memory</span></div></div>
   {usage.resources.length?<div className={styles.resourceList}>{usage.resources.map(item=><div key={item.resource}><span>{item.resource}<small>{usage.trendReady?(item.recentCents>item.previousCents?'Spending increased this week':item.recentCents<item.previousCents?'Spending decreased this week':'No change this week'):'Recorded this period'}</small></span><strong>{dollars(item.cents)}</strong></div>)}</div>:usage.capacity.length?<div className={styles.resourceList}>{usage.capacity.map(item=><div key={item.name}><span>{item.name}<small>{item.hosts} host{item.hosts===1?'':'s'} · {memory(item.memoryMiB)} memory · {item.instances} service instance{item.instances===1?'':'s'}</small></span><small>Unmetered</small></div>)}</div>:<p className={styles.small}>No apps are configured in this workspace yet.</p>}
   <p className={styles.small}>Capacity is the current app configuration. It is not measured CPU or memory utilization.</p>
  </section>
  <section className={styles.forecast} aria-label="Usage forecast"><header><h2>Looking ahead</h2>{forecast?<span>{dollars(projected??0)}</span>:null}</header>
   {forecast?<><p>If future daily usage {change===0?'stays the same':`${change>0?'increases':'decreases'} by ${Math.abs(change)}%`}, your estimated period total is <strong>{dollars(projected??0)}</strong>.</p><label htmlFor="usage-scenario">Future usage <span>{change>0?'+':''}{change}%</span></label><input id="usage-scenario" type="range" min="-100" max="100" step="10" value={change} onChange={event=>setChange(Number(event.target.value))}/><div className={styles.chartAxis}><span>Stops</span><span>Current pace</span><span>Doubles</span></div><p className={styles.small}>Uses your last {forecast.sampleDays} complete days of recorded spending, plus plan fees and credits. This estimate changes no resources or plan settings.</p></>:<><p>A forecast becomes available after at least three complete days with usage records in an active billing period.</p><div className={styles.forecastFactors}><div><span>Could increase</span><p>More compute hours, replicas, stored data, or outbound traffic.</p></div><div><span>Could decrease</span><p>Fewer running resources or less storage and traffic.</p></div></div></>}
  </section>
 </>;
}

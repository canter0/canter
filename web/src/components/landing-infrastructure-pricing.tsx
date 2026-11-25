"use client";

import { useState, type KeyboardEvent, type ReactNode } from "react";
import {
  ciComparison, runnerSizes, sandboxComparison, sandboxHourly, sandboxSizes,
  vpsComparison, vpsMonthly, vpsSizes, type ComparisonRow, type Estimate,
} from "@/lib/infrastructure-pricing";
import styles from "./landing-infrastructure-pricing.module.css";

const products = [
  { id: "sandboxes", label: "Sandboxes", Panel: SandboxPricing },
  { id: "vps", label: "VPS", Panel: VPSPricing },
  { id: "ci", label: "CI", Panel: CIPricing },
] as const;

const number = new Intl.NumberFormat("en-US");
const priceFormatters = new Map<number, Intl.NumberFormat>();

function money(amount: number, digits: number) {
  let formatter = priceFormatters.get(digits);
  if (!formatter) {
    formatter = new Intl.NumberFormat("en-US", {
      style: "currency", currency: "USD", minimumFractionDigits: digits, maximumFractionDigits: digits,
    });
    priceFormatters.set(digits, formatter);
  }
  return formatter.format(amount);
}

function priceLabel(estimate: Estimate, digits: number) {
  const low = money(estimate.low, digits);
  const high = money(estimate.high, digits);
  return low === high ? low : `${low}–${high}`;
}

function sizeOptions(sizes: ReadonlyArray<{ cpu: number; ram: number }>) {
  return sizes.map(({ cpu, ram }, index) => ({
    value: index, label: `${cpu} ${cpu === 1 ? "vCPU" : "vCPUs"} · ${ram} GiB`,
  }));
}

const sandboxOptions = sizeOptions(sandboxSizes);
const vpsOptions = sizeOptions(vpsSizes);
const runnerOptions = sizeOptions(runnerSizes);
const sandboxDurations = [0.5, 1, 4, 8].map(hours => ({
  value: hours, label: hours === 0.5 ? "30 min" : `${hours} h`,
}));
const vpsDurations = [{ value: 24, label: "24 h" }, { value: 168, label: "7 d" }, { value: 720, label: "30 d" }];
const peakOptions = [10, 50, 100, 250].map(value => ({ value, label: String(value) }));
const cpuOptions = [25, 50, 75, 100].map(value => ({ value, label: `${value}%` }));

function Slider({ label, value, min, max, step, onChange }: {
  label: string; value: number; min: number; max: number; step: number; onChange: (value: number) => void;
}) {
  return (
    <label className={styles.setting}>
      <span>{label}</span>
      <span className={styles.slider}>
        <input aria-label={label} type="range" min={min} max={max} step={step} value={value} onChange={event => onChange(event.currentTarget.valueAsNumber)} />
        <span aria-hidden="true">{number.format(value)}</span>
      </span>
    </label>
  );
}

function Choices({ label, options, value, onChange }: {
  label: string; options: { label: string; value: number }[]; value: number; onChange: (value: number) => void;
}) {
  return (
    <div className={styles.setting} role="group" aria-label={label}>
      <span>{label}</span>
      <div className={styles.choices}>
        {options.map(option => (
          <button key={option.value} type="button" aria-pressed={option.value === value} onClick={() => onChange(option.value)}>
            {option.label}
          </button>
        ))}
      </div>
    </div>
  );
}

function RateTable({ title, rateHeading, rows, selected }: {
  title: string; rateHeading: string; rows: { cpu: number; ram: number; rate: string }[]; selected: number;
}) {
  return (
    <table className={styles.rateTable}>
      <caption>{title}</caption>
      <thead>
        <tr><th scope="col">vCPUs</th><th scope="col">RAM</th><th scope="col">{rateHeading}</th></tr>
      </thead>
      <tbody>
        {rows.map(({ cpu, ram, rate }, index) => (
          <tr key={`${cpu}x${ram}`} data-selected={index === selected}>
            <th scope="row">{cpu}{index === selected && <span className="sr-only"> (selected)</span>}</th>
            <td>{ram} GiB</td>
            <td>{rate}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Comparison({ rows, notes }: { rows: ComparisonRow[]; notes: string[] }) {
  const max = Math.max(...rows.map(row => row.high));
  return (
    <div className={styles.comparison}>
      <table className={styles.providers}>
        <caption className="sr-only">Estimated monthly cost by provider in US dollars</caption>
        <colgroup><col className={styles.providerName} /><col /><col className={styles.providerPrice} /></colgroup>
        <thead className="sr-only">
          <tr><th scope="col">Provider</th><th scope="col">Relative cost</th><th scope="col">Estimated cost</th></tr>
        </thead>
        <tbody>
          {rows.map(({ name, source, low, high }) => (
            <tr key={name} data-canter={name === "Canter"}>
              <th scope="row">
                {source ? <a href={source}>{name}</a> : name}
              </th>
              <td aria-hidden="true">
                <span className={styles.bar} style={{ width: `${100 * high / max}%` }} />
              </td>
              <td className={styles.providerCost}>{priceLabel({ low, high }, 2)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className={styles.notes}>
        {notes.map(note => <p key={note}>{note}</p>)}
      </div>
    </div>
  );
}

function Calculator({ controls, total, detail, rateTable, rows, notes }: {
  controls: ReactNode; total: Estimate; detail: string; rateTable: ReactNode; rows: ComparisonRow[]; notes: string[];
}) {
  const digits = Number.isInteger(total.low) && Number.isInteger(total.high) ? 0 : 2;
  const amount = priceLabel(total, digits);
  return (
    <div className={styles.calculator}>
      <div className={styles.configuration}>
        <div className={styles.settings}>{controls}</div>
        <div className={styles.estimate} role="status">
          <p className={styles.price} data-long={amount.length > 13}>
            <span>{amount}</span><span className={styles.period}>/ month</span>
          </p>
          <p className={styles.detail}>{detail}</p>
        </div>
        {rateTable}
      </div>
      <Comparison rows={rows} notes={notes} />
    </div>
  );
}

function SandboxPricing() {
  const [sessions, setSessions] = useState(1000);
  const [duration, setDuration] = useState(0.5);
  const [peak, setPeak] = useState(50);
  const [cpu, setCpu] = useState(100);
  const [selected, select] = useState(0);
  const { total, rows, hours, planNotes } = sandboxComparison(sandboxSizes[selected], sessions, duration, cpu, peak);

  return (
    <Calculator
      controls={<>
        <Slider label="Sessions / month" value={sessions} min={100} max={10000} step={100} onChange={setSessions} />
        <Choices label="Session length" options={sandboxDurations} value={duration} onChange={setDuration} />
        <Choices label="Peak sandboxes" options={peakOptions} value={peak} onChange={setPeak} />
        <Choices label="CPU busy" options={cpuOptions} value={cpu} onChange={setCpu} />
        <Choices label="Per sandbox" options={sandboxOptions} value={selected} onChange={select} />
      </>}
      total={total}
      detail={`${number.format(hours)} running hours · Canter compute estimate`}
      rateTable={<RateTable
        title="Canter sandbox compute" rateHeading="Est. $/hour" selected={selected}
        rows={sandboxSizes.map(shape => ({ ...shape, rate: priceLabel(sandboxHourly(shape), 4) }))}
      />}
      rows={rows}
      notes={[
        "Compute and memory at list rates in USD. Plan fees, credits, storage, and transfer are separate.",
        "CPU busy affects providers that meter CPU use. Canter estimates span both supported tariffs. The live rate is unconfirmed.",
        ...planNotes,
      ]}
    />
  );
}

function VPSPricing() {
  const [instances, setInstances] = useState(1);
  const [hours, setHours] = useState(720);
  const [selected, select] = useState(0);
  const { total, rows } = vpsComparison(vpsSizes[selected], instances, hours);

  return (
    <Calculator
      controls={<>
        <Slider label="Instances" value={instances} min={1} max={50} step={1} onChange={setInstances} />
        <Choices label="Running time" options={vpsDurations} value={hours} onChange={setHours} />
        <Choices label="Per VPS" options={vpsOptions} value={selected} onChange={select} />
      </>}
      total={total}
      detail={`${instances} ${instances === 1 ? "VPS" : "VPS instances"} × ${hours} hours · Linux · pay as you go`}
      rateTable={<RateTable
        title="Canter VPS compute" rateHeading="$/month" selected={selected}
        rows={vpsSizes.map(shape => ({ ...shape, rate: priceLabel(vpsMonthly(shape), 0) }))}
      />}
      rows={rows}
      notes={["Linux compute in USD at the selected CPU and RAM. Regional pricing, disk, IP addresses, and included transfer vary."]}
    />
  );
}

function CIPricing() {
  const [minutes, setMinutes] = useState(1000);
  const [selected, select] = useState(0);
  const { total, rows } = ciComparison(runnerSizes[selected], minutes);

  return (
    <Calculator
      controls={<>
        <Slider label="Paid minutes / month" value={minutes} min={100} max={20000} step={100} onChange={setMinutes} />
        <Choices label="Runner size" options={runnerOptions} value={selected} onChange={select} />
      </>}
      total={total}
      detail={`${number.format(minutes)} paid minutes · Linux x64 · Canter estimate`}
      rateTable={<RateTable
        title="Canter Linux runners" rateHeading="Est. $/min" selected={selected}
        rows={runnerSizes.map(shape => ({ ...shape, rate: money(shape.perMinute, 3) }))}
      />}
      rows={rows}
      notes={["Paid runtime after included minutes. Plan fees vary."]}
    />
  );
}

export function LandingInfrastructurePricing() {
  const [active, setActive] = useState(0);

  function handleTabKey(event: KeyboardEvent<HTMLButtonElement>, index: number) {
    const positions: Record<string, number> = {
      ArrowRight: (index + 1) % products.length,
      ArrowLeft: (index + products.length - 1) % products.length,
      Home: 0,
      End: products.length - 1,
    };
    const next = positions[event.key];
    if (next === undefined) return;
    event.preventDefault();
    setActive(next);
    event.currentTarget.parentElement?.querySelectorAll("button")[next]?.focus();
  }

  return (
    <section className={styles.section} id="compare-costs" aria-labelledby="compare-costs-title">
      <h2 id="compare-costs-title">Compare infrastructure costs</h2>
      <div className={styles.tabs} role="tablist" aria-label="Infrastructure pricing">
        {products.map(({ id, label }, index) => (
          <button
            key={id} type="button" role="tab" id={`cost-tab-${id}`} aria-controls={`cost-panel-${id}`}
            aria-selected={active === index} tabIndex={active === index ? 0 : -1}
            onClick={() => setActive(index)} onKeyDown={event => handleTabKey(event, index)}
          >{label}</button>
        ))}
      </div>
      {products.map(({ id, Panel }, index) => (
        <div key={id} id={`cost-panel-${id}`} role="tabpanel" aria-labelledby={`cost-tab-${id}`} hidden={active !== index}>
          <Panel />
        </div>
      ))}
    </section>
  );
}

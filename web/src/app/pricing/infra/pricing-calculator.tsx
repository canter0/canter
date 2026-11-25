"use client";

import { useState } from "react";
import { WorkspaceIcon } from "@/components/workspace-icon";
import { infraEstimate, infraEstimateLabel, infraSizeLabel, infraWorkloads, isInfraWorkload, vpsRuntimeOptions, type InfraWorkload } from "@/lib/pricing-estimate";
import styles from "../pricing.module.css";

export function PricingCalculator() {
  const [workload, setWorkload] = useState<InfraWorkload>("vps");
  const config = infraWorkloads[workload];
  const [size, setSize] = useState(infraWorkloads.vps.sizes[0].id);
  const [amount, setAmount] = useState(String(infraWorkloads.vps.defaultAmount));
  const estimate = infraEstimate(workload, size, Number(amount));

  function changeWorkload(value: string) {
    if (!isInfraWorkload(value)) return;
    const next = infraWorkloads[value];
    setWorkload(value);
    setSize(next.sizes[0].id);
    setAmount(String(next.defaultAmount));
  }

  return (
    <section className={styles.calculator} aria-labelledby="estimate-title">
      <h2 id="estimate-title">Estimate your setup</h2>
      <form action="/pricing/infra/start" method="get" className={styles.calculatorForm}>
        <div className={styles.fields}>
          <label>
            <span>Workload</span>
            <select name="workload" value={workload} onChange={event => changeWorkload(event.target.value)}>
              {Object.entries(infraWorkloads).map(([id, option]) => <option key={id} value={id}>{option.label}</option>)}
            </select>
          </label>
          <label>
            <span>{workload === "ci" ? "Runner size" : "Size"}</span>
            <select name="size" value={size} onChange={event => setSize(event.target.value)}>
              {config.sizes.map(option => <option key={option.id} value={option.id}>{infraSizeLabel(option)}</option>)}
            </select>
          </label>
          <label>
            <span>{config.amountLabel}</span>
            {workload === "vps" ? (
              <select name="amount" value={amount} onChange={event => setAmount(event.target.value)}>
                {vpsRuntimeOptions.map(hours => <option key={hours} value={hours}>{hours} hours</option>)}
              </select>
            ) : (
              <input name="amount" type="number" inputMode="numeric" value={amount} min={1} max={config.max} step={1} onChange={event => setAmount(event.target.value)} required />
            )}
          </label>
        </div>
        <div className={styles.result}>
          <div role="status">
            <p>Estimated compute</p>
            <div className={styles.estimate}>
              {estimate ? <>{infraEstimateLabel(estimate.total)} <span>/ month</span></> : "—"}
            </div>
          </div>
          <button type="submit" className={styles.primaryButton} disabled={!estimate}>
            Ask Canter <WorkspaceIcon name="external" width={19} height={19} />
          </button>
        </div>
      </form>
      <p className={styles.note}>
        Compute estimate in USD, before tax.
        {workload === "sandboxes" ? " Sandbox rates vary by configuration and tariff." : null}
      </p>
    </section>
  );
}

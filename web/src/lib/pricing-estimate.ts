import {
  ciRuntimeCost, entrySandboxSize, monthHours, runnerSizes, sandboxRuntimeCost, sandboxSizes,
  vpsMonthly, vpsRuntimeCost, vpsSizes, type Estimate,
} from "./infrastructure-pricing";

export const infraWorkloads = {
  sandboxes: { label: "Sandboxes", sizes: [entrySandboxSize, ...sandboxSizes], defaultAmount: 3000, amountLabel: "Awake minutes / month", max: 43200 },
  vps: { label: "VPS", sizes: vpsSizes, defaultAmount: monthHours, amountLabel: "Running time", max: monthHours },
  ci: { label: "CI", sizes: runnerSizes, defaultAmount: 1000, amountLabel: "Paid minutes / month", max: 1000000 },
};

export type InfraWorkload = keyof typeof infraWorkloads;
export const vpsRuntimeOptions = [24, 168, monthHours];

export function isInfraWorkload(value: string): value is InfraWorkload {
  return Object.hasOwn(infraWorkloads, value);
}

export function infraSizeLabel({ cpu, ram }: { cpu: number; ram: number }) {
  return `${cpu} ${cpu === 1 ? "vCPU" : "vCPUs"} · ${ram} GiB`;
}

export function infraEstimate(workload: InfraWorkload, sizeId: string, amount: number) {
  const config = infraWorkloads[workload];
  if (!Number.isSafeInteger(amount) || amount < 1 || amount > config.max) return null;
  if (workload === "vps" && !vpsRuntimeOptions.includes(amount)) return null;
  if (workload === "ci") {
    const size = infraWorkloads.ci.sizes.find(option => option.id === sizeId);
    if (!size) return null;
    return { total: ciRuntimeCost(size, amount), description: `CI with a ${infraSizeLabel(size)} runner and ${amount} paid minutes per month` };
  }
  const size = config.sizes.find(option => option.id === sizeId);
  if (!size) return null;
  if (workload === "sandboxes") {
    return { total: sandboxRuntimeCost(size, amount / 60), description: `a ${infraSizeLabel(size)} sandbox awake for ${amount} minutes per month` };
  }
  return { total: vpsRuntimeCost(size, amount), description: `one ${infraSizeLabel(size)} VPS running for ${amount} hours per month` };
}

function usd(amount: number, maximumFractionDigits: number, minimumFractionDigits = 0) {
  return new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits, maximumFractionDigits }).format(amount);
}

export function infraEstimateLabel({ low, high }: Estimate) {
  const digits = Number.isInteger(low) && Number.isInteger(high) ? 0 : low < 0.01 ? 5 : 2;
  const lowLabel = usd(low, digits, digits);
  const highLabel = usd(high, digits, digits);
  return lowLabel === highLabel ? lowLabel : `${lowLabel}–${highLabel}`;
}

const sandboxMinute = sandboxRuntimeCost(entrySandboxSize, 1 / 60);

export const infraRates = {
  sandboxes: `≈ ${usd((sandboxMinute.low + sandboxMinute.high) / 2, 5)} / awake minute`,
  vps: `≈ ${usd(vpsMonthly(vpsSizes[0]).low / (monthHours * 60), 6)} / minute`,
  ci: `${usd(runnerSizes[0].perMinute, 3)} / paid minute`,
  objects: "$0.014 / GB / month",
};

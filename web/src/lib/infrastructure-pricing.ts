export const monthHours = 720;
// Mirrors the two sandbox tariffs and the VPS rate in pricing/resource.go.
const vpsUnitMonth = 3;
const sandboxUnitMonth = 2.5;

type Size = { id: string; cpu: number; ram: number };
type RunnerSize = Size & { perMinute: number };
type ProviderRate = { name: string; source: string; hourly: number; cap?: number };

export type Estimate = { low: number; high: number };
export type ComparisonRow = Estimate & { name: string; source?: string };

function size(cpu: number, ram: number): Size {
  return { id: `${cpu}x${ram}`, cpu, ram };
}

export const entrySandboxSize = size(1, 2);
export const sandboxSizes = [size(2, 4), size(2, 8), size(4, 16)];
export const vpsSizes = [size(1, 1), size(2, 2), size(2, 4), size(4, 8)];
// Current v2 runner rates in pricing/ci.go.
export const runnerSizes: RunnerSize[] = [
  { ...size(2, 8), perMinute: 0.001 },
  { ...size(4, 16), perMinute: 0.002 },
];

function fixed(amount: number): Estimate {
  return { low: amount, high: amount };
}

function scale(estimate: Estimate, quantity: number): Estimate {
  return { low: estimate.low * quantity, high: estimate.high * quantity };
}

function quote(name: string, source: string, amount: number): ComparisonRow {
  return { name, source, ...fixed(amount) };
}

function byCost(rows: ComparisonRow[]) {
  return rows.sort((a, b) => a.high - b.high);
}

export function sandboxHourly(shape: Size): Estimate {
  const units = Math.max(shape.cpu, shape.ram);
  return scale({ low: sandboxUnitMonth, high: vpsUnitMonth }, units / monthHours);
}

export function vpsMonthly(shape: Size): Estimate {
  return fixed(vpsUnitMonth * Math.max(shape.cpu, shape.ram));
}

export function sandboxRuntimeCost(shape: Size, hours: number) {
  return scale(sandboxHourly(shape), hours);
}

export function vpsRuntimeCost(shape: Size, hours: number) {
  return scale(vpsMonthly(shape), hours / monthHours);
}

export function ciRuntimeCost(shape: RunnerSize, paidMinutes: number) {
  return fixed(shape.perMinute * paidMinutes);
}

export function sandboxComparison(shape: Size, sessions: number, hoursPerSession: number, cpuBusy: number, peak: number) {
  const hours = sessions * hoursPerSession;
  const { cpu, ram } = shape;
  const activeCpu = cpu * cpuBusy / 100;
  const total = sandboxRuntimeCost(shape, hours);
  const rows = [
    quote("Freestyle", "https://www.freestyle.sh/pricing", hours * (0.04032 * cpu + 0.0129 * ram)),
    quote("Daytona", "https://www.daytona.io/pricing", hours * (0.0504 * cpu + 0.0162 * ram)),
    quote("Tensorlake", "https://www.tensorlake.ai/pricing", hours * (0.07 * activeCpu + 0.015 * ram)),
    // Modal counts physical cores (two vCPUs) and bills max(request, usage).
    quote("Modal", "https://modal.com/pricing", hours * 3600 * (0.00003942 * activeCpu / 2 + 0.00000667 * ram)),
    quote("Runloop", "https://runloop.ai/pricing", hours * (0.108 * cpu + 0.0252 * ram)),
    quote("Railway", "https://docs.railway.com/pricing/plans#resource-usage-prices", hours * 50 * (activeCpu + ram) / monthHours),
  ];

  if (ram <= 8) {
    rows.push(quote("E2B", "https://e2b.dev/pricing", hours * 3600 * (0.000014 * cpu + 0.0000045 * ram)));
  }
  if (ram === cpu * 2) {
    rows.push(quote("Vercel", "https://vercel.com/docs/sandbox/pricing", hours * (0.128 * activeCpu + 0.0212 * ram)));
  }
  if (shape.id === "2x8") {
    rows.push(quote("Cloudflare", "https://developers.cloudflare.com/containers/platform/pricing/", hours * 3600 * (0.00002 * activeCpu + 0.0000025 * ram)));
  }
  const hasBoatSize = shape.id === "2x4";
  if (hasBoatSize) rows.push(quote("Boat", "https://docs.boat.dev/pricing", hours * 0.018));

  const planNotes = [];
  if (hasBoatSize) {
    planNotes.push(`Boat’s single-seat plan starts at $${peak > 100 ? 100 : 20}/month for this peak. Starts are subject to separate limits.`);
  }
  if (ram <= 8 && (peak > 10 || hoursPerSession > 1 || shape.id !== "2x4")) {
    planNotes.push(peak > 100
      ? "E2B Pro starts at $150/month and must be expanded for this peak."
      : "E2B charges $150/month for Pro plus usage for this configuration.");
  }
  if (peak > 40) {
    planNotes.push(peak > 200 ? "Freestyle’s published self-serve plans cover up to 200 concurrent VMs." : "Freestyle Pro starts at $500/month for up to 200 concurrent VMs.");
  }

  return { total, hours, rows: [{ name: "Canter", ...total }, ...byCost(rows)], planNotes };
}

const ocean = "https://www.digitalocean.com/pricing/droplets";
const fly = "https://fly.io/docs/about/pricing/";
const linode = "https://api.linode.com/v4/linode/types/";

function droplet(hourly: number, cap: number): ProviderRate {
  return { name: "DigitalOcean", source: ocean, hourly, cap };
}

function nanode(plan: string, hourly: number, cap: number): ProviderRate {
  return { name: "Akamai Linode", source: linode + plan, hourly, cap };
}

const vpsRates: Record<string, ProviderRate[]> = {
  "1x1": [nanode("g6-nanode-1", 0.0075, 5), droplet(0.00893, 6), { name: "Fly.io", source: fly, hourly: 6.70 / monthHours }],
  "2x2": [droplet(0.02679, 18), { name: "Fly.io", source: fly, hourly: 13.39 / monthHours }],
  "2x4": [nanode("g6-standard-2", 0.036, 24), droplet(0.03571, 24)],
  "4x8": [nanode("g6-standard-4", 0.072, 48), droplet(0.07143, 48)],
};

export function vpsComparison(shape: Size, instances: number, hours: number) {
  const total = vpsRuntimeCost(shape, instances * hours);
  const rows = (vpsRates[shape.id] ?? []).map(({ name, source, hourly, cap }) =>
    quote(name, source, instances * Math.min(hours * hourly, cap ?? Infinity)));
  return { total, rows: [{ name: "Canter", source: "/pricing", ...total }, ...byCost(rows)] };
}

const runnerRates = [
  { name: "Sengi Basic", source: "https://sengi.run/pricing", small: 0.001, large: 0.002 },
  { name: "Ubicloud Standard", source: "https://www.ubicloud.com/use-cases/github-actions", small: 0.0012, large: 0.0025 },
  { name: "Blacksmith", source: "https://www.blacksmith.sh/pricing", small: 0.004, large: 0.008 },
  { name: "WarpBuild", source: "https://www.warpbuild.com/docs/ci/cloud-runners", small: 0.004, large: 0.008 },
  { name: "GitHub Actions", source: "https://docs.github.com/en/billing/reference/actions-runner-pricing", small: 0.006, large: 0.012 },
  { name: "Depot", source: "https://depot.dev/pricing", small: 0.006, large: 0.012 },
  { name: "Namespace", source: "https://namespace.so/pricing", small: 0.006, large: 0.012 },
];

export function ciComparison(shape: RunnerSize, paidMinutes: number) {
  const total = ciRuntimeCost(shape, paidMinutes);
  const rate = shape.cpu === 2 ? "small" : "large";
  const rows = runnerRates.map(provider => quote(provider.name, provider.source, paidMinutes * provider[rate]));
  return { total, rows: [{ name: "Canter", ...total }, ...byCost(rows)] };
}

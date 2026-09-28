export type AcquisitionSource = "direct" | "google" | "bing" | "duckduckgo" | "yahoo" | "referral" | "paid" | "campaign";

// Only a fixed category leaves the browser. Never send query strings, referrer
// URLs, campaign values, prompts, authorization links or search terms.
export function acquisitionSource(referrer: string, currentURL: string): AcquisitionSource {
  const current = new URL(currentURL);
  const medium = current.searchParams.get("utm_medium")?.toLowerCase();
  if (["cpc", "ppc", "paid", "paidsearch", "paid_social", "display"].includes(medium ?? "") || ["gclid", "msclkid"].some(key => current.searchParams.has(key))) return "paid";
  if (["email", "social", "newsletter", "affiliate"].includes(medium ?? "")) return "campaign";
  if (!referrer) return "direct";
  try {
    const url = new URL(referrer);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) return "direct";
    const host = url.hostname.toLowerCase();
    if ([current.hostname, "canter.dev", "www.canter.dev"].includes(host)) return "direct";
    if (/^(www\.)?google\.(com|ca|co\.uk|com\.au|de|fr|co\.in|co\.jp)$/.test(host)) return "google";
    if (host === "bing.com" || host === "www.bing.com") return "bing";
    if (host === "duckduckgo.com" || host === "www.duckduckgo.com") return "duckduckgo";
    if (host === "search.yahoo.com") return "yahoo";
    return "referral";
  } catch { return "direct"; }
}

let capture: Promise<void> | undefined;

export function recordAcquisition(landingPath: "/" | "/pricing"): Promise<void> {
  if (capture) return capture;
  const browser = navigator as Navigator & { globalPrivacyControl?: boolean };
  if (browser.doNotTrack === "1" || browser.globalPrivacyControl) return Promise.resolve();
  capture = fetch("/api/canter/acquisition", {
    method: "POST", credentials: "same-origin", keepalive: true,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ landingPath, source: acquisitionSource(document.referrer, window.location.href) }),
    signal: AbortSignal.timeout(2000),
  }).then(() => undefined).catch(() => undefined);
  return capture;
}

// Allow an in-flight first visit to finish before leaving for OAuth or signing
// up. The capture has a two-second timeout and never prevents authentication.
export function finishAcquisition(): Promise<void> {
  return capture ?? Promise.resolve();
}

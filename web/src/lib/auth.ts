export type AuthProvider = { id: "google" | "github"; enabled: boolean; connected: boolean };
export type AuthProviders = { providers: AuthProvider[]; requireInvite: boolean };

export function authDestination(next: string, fallback = "/app") {
  if (!next.startsWith("/") || next.startsWith("//") || /[\\\r\n]/.test(next)) return fallback;
  try {
    const decoded = decodeURIComponent(next);
    if (decoded.startsWith("//") || /[\\\r\n]/.test(decoded)) return fallback;
    const url = new URL(next, "http://canter.local");
    // URL parsing removes dot segments. A path such as "/.//attacker.example"
    // therefore becomes "//attacker.example", which redirect() and the
    // browser treat as an external network-path URL.
    if (url.origin !== "http://canter.local" || url.pathname.startsWith("//")) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch { return fallback; }
}

export function authError(code: string) {
  const messages: Record<string, string> = {
    provider_unavailable: "This sign-in provider is not available yet. Please use your email.",
    session_expired: "Your sign-in session expired. Please try again.",
    access_denied: "Sign-in was cancelled. Please try again when you're ready.",
    sign_in_failed: "We couldn't complete sign-in. Please try again.",
    unverified_email: "Your provider couldn't verify your identity and email address.",
    account_exists: "Verify your email using your existing sign-in method, then try this provider again.",
    access_restricted: "We couldn't grant access. Check your invitation or sign in again.",
  };
  return messages[code] ?? "";
}

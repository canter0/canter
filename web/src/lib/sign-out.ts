import { canterFetch } from "./canter-api";
import { clearAllOperatorAttachmentDrafts } from "./operator-attachment-draft";
import { clearOperatorTextDrafts } from "./operator-draft-storage";

export type SignOutOutcome = { revocationConfirmed: boolean };

// The server confirms whether it actually revoked the session. A rejected
// promise means revocation is unconfirmed; callers keep drafts and offer retry.
export async function requestSignOut(): Promise<SignOutOutcome> {
  const result = await canterFetch<{ signedOut?: boolean; revocationConfirmed?: boolean }>("/auth/signout", { method: "POST" });
  return { revocationConfirmed: result.revocationConfirmed === true };
}

// Local drafts clear only after the server answered the sign-out request.
export async function clearLocalSessionData() {
  try { clearOperatorTextDrafts(sessionStorage); } catch { /* Storage can be unavailable. */ }
  await clearAllOperatorAttachmentDrafts().catch(() => {});
}

export const signOutRecoveryDestination = "/app/account/security";
export const signOutUnconfirmedDescription = "This browser is signed out. To revoke other sessions, sign in again and choose Sign out others in account security.";

// The sign-in page renders the unconfirmed-revocation notice for this flag.
export function signOutUnconfirmedHref(next: string) {
  return `/sign-in?revocation=unconfirmed&next=${encodeURIComponent(next)}`;
}

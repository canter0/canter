import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { LoginShell } from "@/components/login-shell";
import { authDestination } from "@/lib/auth";
import { signOutUnconfirmedDescription } from "@/lib/sign-out";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Sign in" };

export default async function SignInPage({ searchParams }: { searchParams: Promise<{ next?: string; error?: string; deleted?: string; reauth?: string; revocation?: string }> }) {
  const { next = "", error = "", deleted = "", reauth = "", revocation = "" } = await searchParams;
  const confirmIdentity = reauth === "1";
  if (!confirmIdentity) await redirectAuthenticated(authDestination(next));
  const description = confirmIdentity ? "Sign in again to continue where you left off." : revocation === "unconfirmed" ? signOutUnconfirmedDescription : undefined;
  return <LoginShell title={confirmIdentity ? "Confirm your identity" : undefined} description={description}><AuthForm mode="sign-in" next={next} initialError={error} accountDeleted={deleted === "1"} /></LoginShell>;
}

import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { LoginShell } from "@/components/login-shell";
import { authDestination } from "@/lib/auth";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Sign in" };

export default async function SignInPage({ searchParams }: { searchParams: Promise<{ next?: string; error?: string; deleted?: string; reauth?: string }> }) {
  const { next = "", error = "", deleted = "", reauth = "" } = await searchParams;
  const confirmIdentity = reauth === "1";
  if (!confirmIdentity) await redirectAuthenticated(authDestination(next));
  return <LoginShell title={confirmIdentity ? "Confirm your identity" : undefined} description={confirmIdentity ? "Sign in again to continue where you left off." : undefined}><AuthForm mode="sign-in" next={next} initialError={error} accountDeleted={deleted === "1"} /></LoginShell>;
}

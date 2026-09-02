import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { AuthShell, AuthTitle } from "@/components/auth-shell";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Sign in" };

export default async function SignInPage({ searchParams }: { searchParams: Promise<{ next?: string }> }) {
  await redirectAuthenticated();
  const { next = "" } = await searchParams;
  return <AuthShell><AuthTitle title="Sign in." /><AuthForm mode="sign-in" next={next} /></AuthShell>;
}

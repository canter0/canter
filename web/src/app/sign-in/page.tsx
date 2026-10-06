import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { LoginShell } from "@/components/login-shell";
import { authDestination } from "@/lib/auth";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Sign in" };

export default async function SignInPage({ searchParams }: { searchParams: Promise<{ next?: string; error?: string; deleted?: string }> }) {
  const { next = "", error = "", deleted = "" } = await searchParams;
  await redirectAuthenticated(authDestination(next));
  return <LoginShell><AuthForm mode="sign-in" next={next} initialError={error} accountDeleted={deleted === "1"} /></LoginShell>;
}

import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { LoginShell } from "@/components/login-shell";
import { authDestination } from "@/lib/auth";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Create account" };

export default async function CreateAccountPage({ searchParams }: { searchParams: Promise<{ next?: string; error?: string }> }) {
  const { next = "", error = "" } = await searchParams;
  await redirectAuthenticated(authDestination(next));
  return <LoginShell create><AuthForm mode="create-account" next={next} initialError={error} /></LoginShell>;
}

import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { AuthShell, AuthTitle } from "@/components/auth-shell";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Create account" };

export default async function CreateAccountPage({ searchParams }: { searchParams: Promise<{ next?: string }> }) {
  await redirectAuthenticated();
  const { next = "" } = await searchParams;
  return <AuthShell><AuthTitle title="Create account." /><AuthForm mode="create-account" next={next} /></AuthShell>;
}

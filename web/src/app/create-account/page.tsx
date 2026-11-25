import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { LoginShell } from "@/components/login-shell";
import { authDestination } from "@/lib/auth";
import { redirectAuthenticated } from "@/lib/server-auth";

export const metadata: Metadata = { title: "Create account" };

export default async function CreateAccountPage({ searchParams }: { searchParams: Promise<{ next?: string; error?: string }> }) {
  const { next = "", error = "" } = await searchParams;
  await redirectAuthenticated(authDestination(next));
  return (
    <LoginShell create>
      <AuthForm mode="create-account" next={next} initialError={error} />
      <p className="mt-5 text-center text-[13px] leading-relaxed text-[#aaa]">
        By creating an account, you agree to the <a className="underline" href="/terms">Terms of Service</a>. See how we handle your information in the <a className="underline" href="/privacy">Privacy Policy</a>.
      </p>
    </LoginShell>
  );
}

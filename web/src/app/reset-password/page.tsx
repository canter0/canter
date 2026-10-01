import type { Metadata } from "next";
import { AuthForm } from "@/components/auth-form";
import { LoginShell } from "@/components/login-shell";
export const metadata: Metadata = { title: "Reset password" };
export default function ResetPasswordPage() {
  return (
    <LoginShell title="Reset your password" description="Verify your email to recover access">
      <AuthForm mode="reset-password" />
    </LoginShell>
  );
}

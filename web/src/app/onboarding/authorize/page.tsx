import type { Metadata } from "next";
import { AuthorizeAgent } from "@/components/authorize-agent";
import { AuthShell } from "@/components/auth-shell";

export const metadata: Metadata = { title: "Authorize agent" };

export default async function AuthorizeAgentPage({ searchParams }: { searchParams: Promise<{ code?: string }> }) {
  const { code = "" } = await searchParams;
  return (
    <AuthShell>
      <AuthorizeAgent code={code} />
    </AuthShell>
  );
}

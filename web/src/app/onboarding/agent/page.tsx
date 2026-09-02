import type { Metadata } from "next";
import { AuthShell, AuthTitle } from "@/components/auth-shell";
import { AgentConnection } from "@/components/agent-connection";

export const metadata: Metadata = { title: "Bring an agent" };

export default function BringAgentPage() {
  return (
    <AuthShell>
      <AuthTitle title="Bring an agent." description="Ask the agent you already use to connect to Canter." />
      <AgentConnection />
    </AuthShell>
  );
}

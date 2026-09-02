import type { Metadata } from "next";
import { FocusedChangeApproval } from "@/components/focused-change-approval";

export const metadata: Metadata = { title: "Review exact Change" };

export default async function FocusedChangeApprovalPage({ params }: { params: Promise<{ token: string }> }) {
  const { token } = await params;
  return <FocusedChangeApproval token={token} />;
}

import { SystemDetail } from "@/components/system-detail";

export default async function SystemPage({ params }: { params: Promise<{ name: string }> }) {
  const { name } = await params;
  return <SystemDetail name={name} />;
}

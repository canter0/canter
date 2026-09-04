import { ChangeReview } from "@/components/change-review";

export default async function ChangePage({ params, searchParams }: { params: Promise<{ id: string }>; searchParams: Promise<{ system?: string }> }) {
  const [{ id }, { system = "" }] = await Promise.all([params, searchParams]);
  return <ChangeReview id={id} system={system} />;
}

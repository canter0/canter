import { StandingPolicies } from "@/components/standing-policies";

export default async function StandingPoliciesPage({ params, searchParams }: { params: Promise<{ name: string }>; searchParams: Promise<{ from?: string }> }) {
  const [{ name }, { from }] = await Promise.all([params, searchParams]);
  return <StandingPolicies system={name} fromChange={from} />;
}

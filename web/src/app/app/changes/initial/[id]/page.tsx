import { InitialDeploymentReview } from "@/components/initial-deployment-review";

export default async function InitialDeploymentPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <InitialDeploymentReview id={id} />;
}

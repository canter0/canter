import { infraEstimate, isInfraWorkload } from "@/lib/pricing-estimate";
import { startPublicAgentDraft } from "@/lib/public-agent-start";

export async function GET(request: Request) {
  const params = new URL(request.url).searchParams;
  const workload = params.get("workload") ?? "";
  if (!isInfraWorkload(workload)) return new Response(null, { status: 400 });
  const estimate = infraEstimate(workload, params.get("size") ?? "", Number(params.get("amount")));
  if (!estimate) return new Response(null, { status: 400 });

  return startPublicAgentDraft(`Help me estimate the cost of ${estimate.description}. Explain what is included and any additional charges before I decide whether to proceed.`);
}

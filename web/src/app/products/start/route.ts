import { isPublicProductId, publicProducts } from "@/lib/public-products";
import { startPublicAgentDraft } from "@/lib/public-agent-start";

export async function GET(request: Request) {
  const product = new URL(request.url).searchParams.get("product");
  if (!product || !isPublicProductId(product)) return new Response(null, { status: 404 });

  return startPublicAgentDraft(publicProducts[product].prompt);
}

import { WrensHome } from "@/components/wrens-workspace";
import { requireAuthenticated } from "@/lib/server-auth";

export default async function WrensPage() {
  await requireAuthenticated();
  return <WrensHome />;
}

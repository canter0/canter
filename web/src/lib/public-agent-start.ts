import "server-only";
import { randomUUID } from "node:crypto";
import { redirect } from "next/navigation";
import { redirectAuthenticated } from "./server-auth";

export async function startPublicAgentDraft(prompt: string) {
  const query = new URLSearchParams({ compose: "1", draftTab: randomUUID(), prompt });
  const destination = `/app?${query}`;
  await redirectAuthenticated(destination);
  redirect(`/create-account?${new URLSearchParams({ next: destination })}`);
}

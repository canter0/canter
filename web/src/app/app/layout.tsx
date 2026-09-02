import type { ReactNode } from "react";
import { requireAuthenticated } from "@/lib/server-auth";

export default async function AuthenticatedLayout({ children }: { children: ReactNode }) {
  await requireAuthenticated();
  return children;
}

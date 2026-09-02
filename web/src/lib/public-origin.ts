import type { NextRequest } from "next/server";

export function publicOrigin(request: NextRequest): string {
  const configured = process.env.CANTER_PUBLIC_URL?.trim();
  if (!configured) {
    return request.nextUrl.origin;
  }

  const origin = new URL(configured).origin;
  if (!origin.startsWith("http://") && !origin.startsWith("https://")) {
    throw new Error("CANTER_PUBLIC_URL must be an HTTP(S) origin");
  }
  return origin;
}

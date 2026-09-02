import "server-only";

import { cookies } from "next/headers";
import { redirect } from "next/navigation";

const canterAPIOrigin = process.env.CANTER_API_ORIGIN ?? "http://127.0.0.1:8081";

async function authenticated(): Promise<boolean> {
  const store = await cookies();
  const session = store.get("__Host-canter_session") ?? store.get("canter_session");
  if (!session) return false;
  try {
    const response = await fetch(`${canterAPIOrigin}/v1/me`, {
      headers: { cookie: `${session.name}=${session.value}` },
      cache: "no-store",
    });
    return response.ok;
  } catch {
    return false;
  }
}

export async function redirectAuthenticated(destination = "/app") {
  if (await authenticated()) redirect(destination);
}

export async function requireAuthenticated() {
  if (!(await authenticated())) redirect("/sign-in?next=/app");
}

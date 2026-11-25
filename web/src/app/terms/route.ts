import { readFile } from "node:fs/promises";
import path from "node:path";

export const dynamic = "force-static";

export async function GET() {
  const text = await readFile(path.join(process.cwd(), "src/content/terms.md"), "utf8");
  return new Response(text, { headers: { "Content-Type": "text/plain; charset=utf-8" } });
}

import type { NextConfig } from "next";
import path from "node:path";

const canterAPIOrigin = process.env.CANTER_API_ORIGIN ?? "http://127.0.0.1:8081";

const nextConfig: NextConfig = {
  distDir: process.env.CANTER_NEXT_DIST_DIR ?? ".next",
  output: "standalone",
  // The public calculator and control plane share the repository's plan catalog.
  turbopack: { root: path.join(__dirname, "..") },
  poweredByHeader: false,
  // Eight MiB of attachments expand when encoded in the task's JSON body.
  experimental: { proxyClientMaxBodySize: "16mb" },
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" },
        ],
      },
    ];
  },
  async rewrites() {
    return [
      {
        source: "/api/canter/mcp",
        destination: `${canterAPIOrigin}/mcp`,
      },
      {
        source: "/api/canter/v1/:path*",
        destination: `${canterAPIOrigin}/v1/:path*`,
      },
      {
        source: "/api/canter/:path*",
        destination: `${canterAPIOrigin}/v1/:path*`,
      },
    ];
  },
};

export default nextConfig;

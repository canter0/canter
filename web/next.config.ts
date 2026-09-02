import type { NextConfig } from "next";

const canterAPIOrigin = process.env.CANTER_API_ORIGIN ?? "http://127.0.0.1:8081";

const nextConfig: NextConfig = {
  output: "standalone",
  poweredByHeader: false,
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

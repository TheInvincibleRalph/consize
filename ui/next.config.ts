import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /* The browser stays same-origin: /api/v1/* is proxied to the Consize
   * API at dev/build time. Local dev defaults to the local API on :8080.
   * To inspect the live GKE install, opt in with
   * API_UPSTREAM=http://127.0.0.1:18099.
   * NEXT_PUBLIC_API_BASE (see lib/api.ts) takes precedence when the UI
   * is served from somewhere that cannot use these rewrites. */
  async rewrites() {
    const upstream = process.env.API_UPSTREAM || "http://127.0.0.1:8080";
    return [
      {
        source: "/api/v1/:path*",
        destination: `${upstream}/api/v1/:path*`,
      },
    ];
  },
};

export default nextConfig;

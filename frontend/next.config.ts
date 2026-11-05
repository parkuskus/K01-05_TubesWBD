import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  poweredByHeader: false,
  async rewrites() {
    const goOrigin = process.env.GO_API_ORIGIN ?? "http://127.0.0.1:8080";
    return [
      {
        source: "/api/v1/:path*",
        destination: `${goOrigin.replace(/\/$/, "")}/api/v1/:path*`,
      },
    ];
  },
};

export default nextConfig;

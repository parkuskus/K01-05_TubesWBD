import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { Providers } from "./providers";
import "./globals.css";

export const metadata: Metadata = {
  title: { default: "Database — Teras Data", template: "%s — Teras Data" },
  description: "Workspace untuk mengelola koneksi PostgreSQL dan aktivitas migration.",
  robots: { index: false, follow: false },
};

export const viewport: Viewport = { themeColor: "#f6f5f1" };

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="id">
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}

import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { AppShell } from "@/components/app-shell";
import { Providers } from "./providers";
import { WorkspaceProvider } from "@/components/workspace-provider";
import { getWorkspaceSnapshot } from "@/lib/workspace/source";
import "./globals.css";

// Workspace data belongs to each request; it must not be baked into a static page.
export const dynamic = "force-dynamic";

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
        <Providers>
          <WorkspaceProvider snapshot={getWorkspaceSnapshot()}>
            <AppShell>{children}</AppShell>
          </WorkspaceProvider>
        </Providers>
      </body>
    </html>
  );
}

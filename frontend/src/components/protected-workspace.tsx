"use client";

import { usePathname, useRouter } from "next/navigation";
import { useEffect, type ReactNode } from "react";
import { AppShell } from "@/components/app-shell";
import { useAuth } from "@/components/auth-provider";
import { SessionState } from "@/components/session-state";
import { WorkspaceProvider } from "@/components/workspace-provider";

export function ProtectedWorkspace({ children }: { children: ReactNode }) {
  const { status } = useAuth();
  const router = useRouter();
  const pathname = usePathname();

  useEffect(() => {
    if (status === "anonymous") {
      const destination = `${pathname}${window.location.search}`;
      router.replace(`/login?next=${encodeURIComponent(destination)}`);
    }
  }, [pathname, router, status]);

  if (status !== "authenticated") return <SessionState />;
  return (
    <WorkspaceProvider>
      <AppShell>{children}</AppShell>
    </WorkspaceProvider>
  );
}

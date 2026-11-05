"use client";

import { createContext, useContext, type ReactNode } from "react";
import type { WorkspaceSnapshot } from "@/lib/workspace/types";

const WorkspaceContext = createContext<WorkspaceSnapshot | null>(null);

export function WorkspaceProvider({
  snapshot,
  children,
}: {
  snapshot: WorkspaceSnapshot;
  children: ReactNode;
}) {
  return <WorkspaceContext.Provider value={snapshot}>{children}</WorkspaceContext.Provider>;
}

export function useWorkspace() {
  const workspace = useContext(WorkspaceContext);
  if (!workspace) throw new Error("WorkspaceProvider is required.");
  return workspace;
}

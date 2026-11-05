import { workspaceFixture } from "@/lib/fixtures/workspace";
import type { WorkspaceSnapshot } from "./types";

// Server entry point used by the root layout. Replace this boundary with the
// agreed Go API adapter; never use fixtures to recover from API failures.
export function getWorkspaceSnapshot(): WorkspaceSnapshot {
  const useLocalData =
    process.env.WORKSPACE_DATA_SOURCE === "local" ||
    (process.env.NODE_ENV === "development" &&
      process.env.WORKSPACE_DATA_SOURCE !== "disconnected");
  if (useLocalData) return workspaceFixture;
  return { source: "disconnected", databases: [], activity: [] };
}

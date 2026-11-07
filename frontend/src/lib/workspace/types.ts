import type { JobStatus } from "@/lib/api/types";
import type { DatabaseChoice } from "@/lib/api/databases";

export type ActivityItem = {
  id: string;
  name: string;
  targetId: string;
  target: string;
  direction: "APPLY" | "ROLLBACK" | "FORWARD";
  status: JobStatus;
  createdAt: string;
  completedBatches?: number;
  totalBatches?: number;
  error?: string;
};

export type WorkspaceSnapshot = {
  source: "connected" | "disconnected";
  databases: DatabaseChoice[];
  activity: ActivityItem[];
};

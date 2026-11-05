import type { JobStatus } from "@/lib/api/types";

export type DatabaseSummary = {
  id: string;
  name: string;
  host: string;
  port: number;
  database: string;
  environment: "Development" | "Staging" | "Production";
  schemas: number;
  tables: number;
  access: "View + Edit" | "View Only";
};

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
  databases: DatabaseSummary[];
  activity: ActivityItem[];
};

// UI input model, mapped to a backend DTO by the future connection adapter.
export type ConnectionInput = {
  name: string;
  host: string;
  port: number;
  database: string;
  username: string;
  password: string;
  sslmode: "require" | "disable" | "verify-full";
};

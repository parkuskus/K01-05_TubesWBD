import { ApiError } from "@/lib/api/client";
import type { Direction } from "@/lib/api/migrations";

export const directionLabels: Record<Direction, string> = {
  APPLY: "Apply",
  ROLLBACK: "Rollback",
  FORWARD: "Forward",
  RESUME: "Lanjutkan",
  COMPENSATE: "Kompensasi",
};

export function pollInterval(status: string | undefined, error: Error | null, failures = 0) {
  if (error instanceof ApiError && [401, 403, 404, 501].includes(error.status)) return false;
  if (error) return Math.min(30_000, 2_000 * 2 ** Math.min(failures, 4));
  if (["SUCCESS", "FAILED", "REJECTED", "STALE", "CANCELLED"].includes(status ?? "")) return false;
  return 2_000;
}

const dateFormatter = new Intl.DateTimeFormat("id-ID", {
  dateStyle: "medium",
  timeStyle: "short",
  timeZone: "Asia/Jakarta",
});

export function formatTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "—" : `${dateFormatter.format(date)} WIB`;
}

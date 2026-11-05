import { afterEach, expect, it, vi } from "vitest";
import { getWorkspaceSnapshot } from "./source";

afterEach(() => vi.unstubAllEnvs());

it("uses empty disconnected data by default in production", () => {
  vi.stubEnv("NODE_ENV", "production");
  vi.stubEnv("WORKSPACE_DATA_SOURCE", undefined);
  expect(getWorkspaceSnapshot()).toEqual({ source: "disconnected", databases: [], activity: [] });
});

it("identifies fixtures as local when explicitly enabled", () => {
  vi.stubEnv("NODE_ENV", "production");
  vi.stubEnv("WORKSPACE_DATA_SOURCE", "local");
  const snapshot = getWorkspaceSnapshot();
  expect(snapshot.source).toBe("local");
  expect(snapshot.databases.length).toBeGreaterThan(0);
});

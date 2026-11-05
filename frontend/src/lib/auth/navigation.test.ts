import { expect, it } from "vitest";
import { loginDestination } from "./navigation";

it.each([
  null,
  "https://attacker.example",
  "//attacker.example",
  "/\\attacker.example",
  "/login",
  "/a/../login",
  "/api/v1/auth/logout",
  "/\nattacker.example",
])("rejects unsafe login destinations: %s", (path) => expect(loginDestination(path)).toBe("/"));

it("preserves an internal route and its query", () => {
  expect(loginDestination("/activity?status=FAILED")).toBe("/activity?status=FAILED");
});

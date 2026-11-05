import type { ReactNode } from "react";
import { ProtectedWorkspace } from "@/components/protected-workspace";

export default function WorkspaceLayout({ children }: { children: ReactNode }) {
  return <ProtectedWorkspace>{children}</ProtectedWorkspace>;
}

import type { Metadata } from "next";
import { Suspense } from "react";
import { LoginScreen } from "@/components/login-screen";
import { SessionState } from "@/components/session-state";

export const metadata: Metadata = { title: "Masuk" };

export default function LoginPage() {
  return (
    <Suspense fallback={<SessionState />}>
      <LoginScreen />
    </Suspense>
  );
}

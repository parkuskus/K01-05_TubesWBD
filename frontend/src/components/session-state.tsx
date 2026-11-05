"use client";

import { useAuth } from "@/components/auth-provider";
import { Brand } from "@/components/brand";
import { Button } from "@/components/ui/button";

export function SessionState() {
  const { status, refreshSession } = useAuth();
  return (
    <main className="grid min-h-dvh place-items-center bg-ground px-4 py-12">
      <div className="flex max-w-sm flex-col items-center text-center">
        <Brand />
        {status === "error" ? (
          <>
            <h1 className="mt-10 text-lg font-semibold">Layanan sementara tidak tersedia</h1>
            <p role="alert" className="mt-3 text-sm leading-6 text-ink-secondary">
              Sesi Anda belum dapat diperiksa. Coba lagi dalam beberapa saat.
            </p>
            <Button className="mt-6" onClick={() => void refreshSession()}>
              Coba lagi
            </Button>
          </>
        ) : (
          <p role="status" className="mt-8 text-sm text-ink-secondary">
            Memuat sesi…
          </p>
        )}
      </div>
    </main>
  );
}

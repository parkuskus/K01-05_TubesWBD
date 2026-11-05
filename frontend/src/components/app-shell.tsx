"use client";

import * as DialogPrimitive from "@radix-ui/react-dialog";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Activity, ChevronRight, Database, Menu, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useWorkspace } from "@/components/workspace-provider";
import { cx } from "@/lib/utils";

const navigation = [
  { label: "Database", href: "/", icon: Database },
  { label: "Aktivitas", href: "/activity", icon: Activity },
];
const desktopMediaQuery = "(min-width: 64rem)";

function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const pathname = usePathname();
  const { source } = useWorkspace();
  return (
    <div className="flex h-full flex-col bg-ground px-4 py-6">
      <Link href="/" onClick={onNavigate} className="flex min-h-11 items-center gap-2.5 px-3">
        <svg viewBox="0 0 28 28" className="size-7 text-accent" fill="none" aria-hidden="true">
          <rect x="2" y="2" width="24" height="24" rx="6" fill="currentColor" />
          <path d="M8 9h12M14 9v11" stroke="white" strokeWidth="2.5" />
        </svg>
        <span className="text-[17px] font-semibold tracking-[-0.04em]">teras data</span>
      </Link>
      <div className="mx-3 mt-9 mb-3 text-xs font-medium text-ink-secondary">Workspace</div>
      <nav aria-label="Navigasi utama" className="space-y-1">
        {navigation.map(({ label, href, icon: Icon }) => {
          const active = pathname === href;
          return (
            <Link
              key={href}
              href={href}
              onClick={onNavigate}
              aria-current={active ? "page" : undefined}
              className={cx(
                "flex min-h-11 items-center gap-3 rounded-md px-3 text-sm transition-colors",
                active
                  ? "bg-accent-soft font-medium text-accent"
                  : "text-ink-secondary hover:bg-field-subtle hover:text-ink",
              )}
            >
              <Icon className="size-4" strokeWidth={1.7} aria-hidden="true" />
              {label}
            </Link>
          );
        })}
      </nav>
      <div className="mt-auto px-3 pt-8 pb-2">
        <div className="flex items-center gap-2 text-xs text-ink-secondary">
          <span
            className={cx(
              "size-1.5 rounded-full",
              source === "connected" ? "bg-accent" : "bg-control-border",
            )}
            aria-hidden="true"
          />
          {source === "local"
            ? "Data lokal"
            : source === "connected"
              ? "Terhubung"
              : "Belum terhubung"}
        </div>
      </div>
    </div>
  );
}

export function AppShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const [menuOpen, setMenuOpen] = useState(false);
  const mainRef = useRef<HTMLElement>(null);

  useEffect(() => {
    const desktop = window.matchMedia(desktopMediaQuery);
    const closeOnDesktop = (event: MediaQueryListEvent) => {
      if (event.matches) setMenuOpen(false);
    };
    desktop.addEventListener("change", closeOnDesktop);
    return () => desktop.removeEventListener("change", closeOnDesktop);
  }, []);

  return (
    <div className="min-h-dvh bg-surface">
      <a
        href="#main-content"
        className="fixed top-2 left-2 z-[100] -translate-y-24 rounded-md bg-accent px-4 py-3 text-sm text-surface focus:translate-y-0"
      >
        Lewati ke konten utama
      </a>
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-56 border-r border-line lg:block">
        <Sidebar />
      </aside>
      <div className="min-w-0 lg:pl-56">
        <header className="flex min-h-17 items-center gap-3 border-b border-line-soft bg-surface px-4 sm:px-8 lg:px-10">
          <DialogPrimitive.Root open={menuOpen} onOpenChange={setMenuOpen}>
            <DialogPrimitive.Trigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="-ml-2 lg:hidden"
                aria-label="Buka navigasi"
              >
                <Menu aria-hidden="true" />
              </Button>
            </DialogPrimitive.Trigger>
            <DialogPrimitive.Portal>
              <DialogPrimitive.Overlay className="fixed inset-0 z-40 bg-ink/35 lg:hidden" />
              <DialogPrimitive.Content
                className="fixed inset-y-0 left-0 z-50 w-[min(20rem,90vw)] overflow-y-auto bg-ground lg:hidden"
                onCloseAutoFocus={(event) => {
                  if (window.matchMedia(desktopMediaQuery).matches) {
                    event.preventDefault();
                    mainRef.current?.focus();
                  }
                }}
              >
                <DialogPrimitive.Title className="sr-only">
                  Navigasi Teras Data
                </DialogPrimitive.Title>
                <DialogPrimitive.Description className="sr-only">
                  Pilih halaman workspace.
                </DialogPrimitive.Description>
                <Sidebar onNavigate={() => setMenuOpen(false)} />
                <DialogPrimitive.Close
                  aria-label="Tutup navigasi"
                  className="absolute top-5 right-3 grid size-11 place-items-center rounded-md text-ink-secondary hover:bg-field"
                >
                  <X className="size-4" aria-hidden="true" />
                </DialogPrimitive.Close>
              </DialogPrimitive.Content>
            </DialogPrimitive.Portal>
          </DialogPrimitive.Root>
          <span className="text-sm text-ink-secondary">Workspace</span>
          <ChevronRight className="size-3 text-ink-secondary" aria-hidden="true" />
          <span className="text-sm font-medium">
            {pathname === "/activity" ? "Aktivitas" : pathname === "/" ? "Database" : "Halaman"}
          </span>
        </header>
        <main
          ref={mainRef}
          id="main-content"
          tabIndex={-1}
          className="mx-auto max-w-[1360px] px-4 py-8 outline-none sm:px-8 sm:py-10 lg:px-10 xl:px-14"
        >
          {children}
        </main>
      </div>
    </div>
  );
}

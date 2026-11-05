"use client";

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { ReactNode } from "react";
import { buttonStyles } from "./button";

export function Dialog({
  trigger,
  title,
  description,
  children,
  open,
  onOpenChange,
}: {
  trigger: ReactNode;
  title: string;
  description: string;
  children?: ReactNode;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Trigger asChild>{trigger}</DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="fixed inset-0 z-50 grid grid-cols-[minmax(0,1fr)] place-items-center overflow-y-auto bg-ink/35 p-4">
          <DialogPrimitive.Content className="relative max-h-[90dvh] w-full max-w-lg min-w-0 overflow-y-auto rounded-lg border border-line bg-surface p-6 shadow-lg sm:p-7">
            <DialogPrimitive.Title className="pr-8 text-lg font-semibold tracking-tight [overflow-wrap:anywhere]">
              {title}
            </DialogPrimitive.Title>
            <DialogPrimitive.Description className="mt-2 text-sm leading-6 text-ink-secondary">
              {description}
            </DialogPrimitive.Description>
            {children && <div className="mt-6">{children}</div>}
            <DialogPrimitive.Close
              className={buttonStyles({
                variant: "ghost",
                size: "icon",
                className: "absolute top-3 right-3",
              })}
              aria-label="Tutup dialog"
            >
              <X aria-hidden="true" />
            </DialogPrimitive.Close>
          </DialogPrimitive.Content>
        </DialogPrimitive.Overlay>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

import { ChevronDown, Search } from "lucide-react";
import type { ComponentProps, ReactNode } from "react";
import { cx } from "@/lib/utils";

export function PageHeading({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <div className="mb-9 flex flex-wrap items-start justify-between gap-5">
      <div>
        <h1 className="text-2xl font-semibold tracking-[-0.035em]">{title}</h1>
        <p className="mt-2 text-sm leading-6 text-ink-secondary">{description}</p>
      </div>
      {action}
    </div>
  );
}

export function SearchInput({
  id,
  label,
  className,
  ...props
}: ComponentProps<"input"> & { id: string; label: string }) {
  return (
    <div className={cx("relative min-w-0", className)}>
      <label htmlFor={id} className="sr-only">
        {label}
      </label>
      <Search
        className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-ink-secondary"
        strokeWidth={1.7}
        aria-hidden="true"
      />
      <input
        {...props}
        type="search"
        id={id}
        className="min-h-11 w-full rounded-md border border-control-border bg-surface pr-3 pl-9 text-base placeholder:text-ink-secondary sm:text-sm"
      />
    </div>
  );
}

export function FilterSelect({
  id,
  label,
  children,
  ...props
}: ComponentProps<"select"> & { id: string; label: string }) {
  return (
    <div className="relative max-w-full">
      <label htmlFor={id} className="sr-only">
        {label}
      </label>
      <select
        {...props}
        id={id}
        className="min-h-11 w-full appearance-none rounded-md border border-control-border bg-surface py-2 pr-10 pl-3 text-base text-ink sm:text-sm"
      >
        {children}
      </select>
      <ChevronDown
        className="pointer-events-none absolute top-1/2 right-3 size-3.5 -translate-y-1/2 text-ink-secondary"
        aria-hidden="true"
      />
    </div>
  );
}

export function TableViewport({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div
      role="region"
      aria-label={label}
      tabIndex={0}
      className="min-w-0 overflow-x-auto rounded-md border border-line"
    >
      {children}
    </div>
  );
}

export function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[minmax(100px,0.7fr)_minmax(0,1fr)] gap-5 py-3 text-sm">
      <dt className="text-ink-secondary">{label}</dt>
      <dd className="min-w-0 [overflow-wrap:anywhere]">{children}</dd>
    </div>
  );
}

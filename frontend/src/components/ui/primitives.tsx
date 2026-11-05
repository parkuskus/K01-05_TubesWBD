import type { ComponentProps, ReactNode } from "react";
import { CircleCheck, CircleDot, Clock3, TriangleAlert } from "lucide-react";
import type { JobStatus } from "@/lib/api/types";
import { cx } from "@/lib/utils";

export function Badge({
  children,
  tone = "neutral",
  className,
}: {
  children: ReactNode;
  tone?: "neutral" | "success" | "warning" | "danger";
  className?: string;
}) {
  return (
    <span
      className={cx(
        "inline-flex max-w-full items-center gap-1.5 rounded px-2 py-1 text-xs leading-4 font-medium [&_svg]:size-3",
        tone === "neutral" && "bg-field-subtle text-ink-secondary",
        tone === "success" && "bg-accent-soft text-accent",
        tone === "warning" && "bg-warning-surface text-warning",
        tone === "danger" && "bg-danger-surface text-danger",
        className,
      )}
    >
      {children}
    </span>
  );
}

const jobStates: Record<
  JobStatus,
  { label: string; tone: "neutral" | "success" | "warning" | "danger"; icon: typeof Clock3 }
> = {
  PENDING: { label: "Dalam antrean", tone: "neutral", icon: Clock3 },
  RUNNING: { label: "Berjalan", tone: "success", icon: CircleDot },
  RETRY_WAIT: { label: "Menunggu retry", tone: "warning", icon: Clock3 },
  BLOCKED: { label: "Tertahan", tone: "warning", icon: TriangleAlert },
  PARTIAL: { label: "Sebagian selesai", tone: "warning", icon: CircleDot },
  PARTIALLY_REVERTED: { label: "Sebagian dibatalkan", tone: "warning", icon: CircleDot },
  UNKNOWN: { label: "Perlu pemeriksaan", tone: "warning", icon: TriangleAlert },
  SUCCESS: { label: "Berhasil", tone: "success", icon: CircleCheck },
  FAILED: { label: "Gagal", tone: "danger", icon: TriangleAlert },
};

export function StatusBadge({ status }: { status: JobStatus }) {
  const { label, tone, icon: Icon } = jobStates[status];
  return (
    <Badge tone={tone}>
      <Icon aria-hidden="true" />
      {label}
    </Badge>
  );
}

export function Panel({ className, ...props }: ComponentProps<"section">) {
  return <section className={cx("surface-panel min-w-0", className)} {...props} />;
}

export function PanelHeader({
  eyebrow,
  title,
  action,
}: {
  eyebrow?: string;
  title: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line-soft px-5 py-4 sm:px-6">
      <div>
        {eyebrow && <p className="eyebrow mb-1 text-ink-secondary">{eyebrow}</p>}
        <h2 className="text-base font-semibold tracking-tight">{title}</h2>
      </div>
      {action}
    </div>
  );
}

export function ProgressBar({ value, label }: { value: number; label: string }) {
  const percent = Math.max(0, Math.min(100, value));
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuenow={percent}
      aria-valuemin={0}
      aria-valuemax={100}
      className="h-2.5 overflow-hidden rounded-xs border border-line bg-field"
    >
      <div
        className="h-full w-full origin-left bg-accent transition-transform duration-200 motion-reduce:transition-none"
        style={{ transform: `scaleX(${percent / 100})` }}
      />
    </div>
  );
}

export function TextField({
  label,
  hint,
  error,
  className,
  id,
  ...props
}: ComponentProps<"input"> & { id: string; label: string; hint?: string; error?: string }) {
  const description = error || hint;
  return (
    <div className={cx("grid min-w-0 gap-2", className)}>
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      <input
        {...props}
        id={id}
        aria-invalid={error ? true : undefined}
        aria-describedby={description ? `${id}-description` : undefined}
        className={cx(
          "min-h-11 w-full min-w-0 rounded-md border bg-surface px-3 text-base text-ink placeholder:text-ink-secondary/80 sm:text-sm",
          error ? "border-danger" : "border-control-border",
        )}
      />
      {description && (
        <p
          id={`${id}-description`}
          className={cx("text-xs leading-relaxed", error ? "text-danger" : "text-ink-secondary")}
        >
          {description}
        </p>
      )}
    </div>
  );
}

export function SqlField({
  id,
  label,
  hint,
  error,
  className,
  ...props
}: ComponentProps<"textarea"> & { id: string; label: string; hint?: string; error?: string }) {
  const description = error || hint;
  return (
    <div className="grid min-w-0 gap-2">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      <textarea
        {...props}
        id={id}
        wrap="off"
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        aria-invalid={error ? true : undefined}
        aria-describedby={description ? `${id}-description` : undefined}
        className={cx(
          "min-h-40 w-full min-w-0 resize-y overflow-auto rounded-md border bg-ground p-4 font-mono text-base leading-7 text-ink sm:text-sm",
          error ? "border-danger" : "border-control-border",
          className,
        )}
      />
      {description && (
        <p
          id={`${id}-description`}
          className={cx("text-xs leading-relaxed", error ? "text-danger" : "text-ink-secondary")}
        >
          {description}
        </p>
      )}
    </div>
  );
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center px-6 py-12 text-center">
      <h3 className="text-sm font-semibold">{title}</h3>
      <p className="mt-2 max-w-sm text-sm leading-relaxed text-ink-secondary">{description}</p>
      {action && <div className="mt-5">{action}</div>}
    </div>
  );
}

export function LoadingState() {
  return (
    <div role="status" className="space-y-4 p-6">
      <span className="sr-only">Memuat data…</span>
      {["w-2/3", "w-full", "w-5/6"].map((width) => (
        <div
          key={width}
          aria-hidden="true"
          className={cx("h-5 animate-pulse rounded bg-field motion-reduce:animate-none", width)}
        />
      ))}
    </div>
  );
}

import type { ComponentProps } from "react";
import { cx } from "@/lib/utils";

type Variant = "primary" | "secondary" | "ghost" | "danger";
type Size = "default" | "small" | "icon";

const variants: Record<Variant, string> = {
  primary: "border-accent bg-accent text-accent-foreground hover:bg-accent/90 active:bg-accent/80",
  secondary: "border-line bg-surface text-ink hover:border-control-border hover:bg-field-subtle",
  ghost: "border-transparent bg-transparent text-ink-secondary hover:bg-field hover:text-ink",
  danger: "border-danger bg-danger text-surface hover:bg-danger/90",
};

export function buttonStyles({
  variant = "primary",
  size = "default",
  className,
}: {
  variant?: Variant;
  size?: Size;
  className?: string;
} = {}) {
  return cx(
    "inline-flex shrink-0 items-center justify-center gap-2 rounded-md border font-medium transition-colors duration-150 disabled:pointer-events-none disabled:opacity-45 [&_svg]:size-4 [&_svg]:shrink-0",
    size === "default" && "min-h-11 px-4 text-sm",
    size === "small" && "min-h-11 px-3 text-xs",
    size === "icon" && "size-11",
    variants[variant],
    className,
  );
}

export function Button({
  variant,
  size,
  className,
  type = "button",
  ...props
}: ComponentProps<"button"> & { variant?: Variant; size?: Size }) {
  return <button type={type} className={buttonStyles({ variant, size, className })} {...props} />;
}

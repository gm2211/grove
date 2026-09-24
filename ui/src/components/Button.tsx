import type { ButtonHTMLAttributes } from "react";

type Variant = "default" | "primary" | "danger" | "ghost";
type Size = "md" | "sm" | "icon";

const VARIANT_CLASS: Record<Variant, string> = {
  default:
    "border border-[var(--border-strong)] bg-[var(--bg-elevated)] text-[var(--fg)] shadow-[var(--shadow-sm)] hover:bg-[var(--bg-hover)]",
  primary:
    "border border-transparent bg-[var(--accent)] text-[var(--accent-fg)] shadow-[var(--shadow-sm)] hover:bg-[var(--accent-hover)]",
  danger:
    "border border-transparent bg-[var(--status-failed)] text-white shadow-[var(--shadow-sm)] hover:brightness-110",
  ghost: "border border-transparent text-[var(--fg-muted)] hover:bg-[var(--bg-hover)] hover:text-[var(--fg)]",
};

const SIZE_CLASS: Record<Size, string> = {
  md: "h-8 rounded-lg px-3 text-[13px] gap-1.5",
  sm: "h-7 rounded-md px-2.5 text-xs gap-1",
  icon: "h-7 w-7 rounded-md justify-center",
};

export function Button({
  variant = "default",
  size = "md",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: Size }) {
  return (
    <button
      {...props}
      className={`inline-flex shrink-0 items-center font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${SIZE_CLASS[size]} ${VARIANT_CLASS[variant]} ${className}`}
    />
  );
}

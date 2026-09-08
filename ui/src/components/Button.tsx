import type { ButtonHTMLAttributes } from "react";

type Variant = "default" | "primary" | "danger";
type Size = "md" | "sm" | "icon";

const VARIANT_STYLE: Record<Variant, string> = {
  default: "border",
  primary: "border-transparent",
  danger: "border-transparent",
};

const SIZE_STYLE: Record<Size, string> = {
  md: "rounded px-3 py-1.5 text-sm",
  sm: "rounded px-2 py-1 text-xs",
  icon: "rounded flex h-6 w-6 items-center justify-center",
};

export function Button({
  variant = "default",
  size = "md",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: Size }) {
  const style: Record<string, string> = {};
  if (variant === "default") {
    style.borderColor = "var(--border)";
    style.background = "var(--bg-elevated)";
    style.color = "var(--fg)";
  } else if (variant === "primary") {
    style.background = "var(--accent)";
    style.color = "var(--accent-fg)";
  } else if (variant === "danger") {
    style.background = "var(--status-failed)";
    style.color = "#fff";
  }
  return (
    <button
      {...props}
      className={`font-medium disabled:cursor-not-allowed disabled:opacity-50 ${SIZE_STYLE[size]} ${VARIANT_STYLE[variant]} ${className}`}
      style={style}
    />
  );
}

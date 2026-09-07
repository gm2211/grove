import type { ButtonHTMLAttributes } from "react";

type Variant = "default" | "primary" | "danger";

const VARIANT_STYLE: Record<Variant, string> = {
  default: "border",
  primary: "border-transparent",
  danger: "border-transparent",
};

export function Button({
  variant = "default",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant }) {
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
      className={`rounded px-3 py-1.5 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 ${VARIANT_STYLE[variant]} ${className}`}
      style={style}
    />
  );
}

import type { ButtonHTMLAttributes } from "react";

type Variant = "default" | "primary" | "solid" | "danger";
type Size = "md" | "sm" | "icon";

const VARIANT_CLASS: Record<Variant, string> = {
  default: "",
  primary: "act-rec",
  solid: "act-solid",
  danger: "act-warn",
};

/** The Argos `.act` button: mono, uppercase, hairline border. */
export function Button({
  variant = "default",
  size = "md",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: Size }) {
  if (size === "icon") {
    return <button {...props} className={`iconbtn ${className}`} />;
  }
  return <button {...props} className={`act ${VARIANT_CLASS[variant]} ${size === "sm" ? "sm" : ""} ${className}`} />;
}

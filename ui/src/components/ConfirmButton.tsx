import { useState, type ReactNode } from "react";
import { Button } from "./Button";

/** A button that requires a second click within a few seconds to actually fire. */
export function ConfirmButton({
  label,
  confirmLabel = "confirm?",
  onConfirm,
  variant = "danger",
  size = "md",
  disabled,
  className,
  title,
}: {
  label: ReactNode;
  confirmLabel?: ReactNode;
  onConfirm: () => void;
  variant?: "danger" | "default" | "primary";
  size?: "md" | "sm" | "icon";
  disabled?: boolean;
  className?: string;
  title?: string;
}) {
  const [armed, setArmed] = useState(false);

  return (
    <Button
      type="button"
      variant={armed ? "danger" : variant}
      size={armed && size === "icon" ? "sm" : size}
      disabled={disabled}
      className={className}
      title={title}
      onClick={() => {
        if (armed) {
          setArmed(false);
          onConfirm();
        } else {
          setArmed(true);
          setTimeout(() => setArmed(false), 3000);
        }
      }}
    >
      {armed ? confirmLabel : label}
    </Button>
  );
}

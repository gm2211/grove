import { useEffect, useRef, useState, type ReactNode } from "react";

/** A button that needs a second click within a few seconds to actually fire. */
export function ConfirmButton({
  label,
  confirmLabel = "sure?",
  onConfirm,
  size = "md",
  disabled,
  className = "",
  title,
}: {
  label: ReactNode;
  confirmLabel?: ReactNode;
  onConfirm: () => void;
  size?: "md" | "sm";
  disabled?: boolean;
  className?: string;
  title?: string;
}) {
  const [armed, setArmed] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  return (
    <button
      type="button"
      className={`act ${armed ? "act-armed" : "act-warn"} ${size === "sm" ? "sm" : ""} ${className}`}
      disabled={disabled}
      title={title}
      onClick={() => {
        clearTimeout(timer.current);
        if (armed) {
          setArmed(false);
          onConfirm();
        } else {
          setArmed(true);
          timer.current = setTimeout(() => setArmed(false), 3000);
        }
      }}
    >
      {armed ? confirmLabel : label}
    </button>
  );
}

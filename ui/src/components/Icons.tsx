import type { SVGProps } from "react";

// Small hand-drawn 16px stroke icons, so the UI keeps its "no UI kit" dependency footprint.
type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function Svg({ size = 16, children, ...props }: IconProps) {
  return (
    <svg
      viewBox="0 0 16 16"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...props}
    >
      {children}
    </svg>
  );
}

export function GroveMark(props: IconProps) {
  return (
    <Svg {...props} strokeWidth="1.5">
      <path d="M8 14.5V8" />
      <path d="M8 8c0-3 1.8-5.5 5-6-0.2 3.4-2.2 5.6-5 6Z" fill="currentColor" fillOpacity="0.18" />
      <path d="M8 10.5C8 8 6.5 6 3.5 5.6 3.6 8.4 5.4 10.3 8 10.5Z" fill="currentColor" fillOpacity="0.18" />
    </Svg>
  );
}

export function FleetIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="2" y="2.5" width="12" height="4.5" rx="1.2" />
      <rect x="2" y="9" width="12" height="4.5" rx="1.2" />
      <path d="M4.5 4.75h.01M4.5 11.25h.01" strokeWidth="2" />
    </Svg>
  );
}

export function JobsIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M5.5 4h8M5.5 8h8M5.5 12h8" />
      <path d="M2.5 4h.01M2.5 8h.01M2.5 12h.01" strokeWidth="2" />
    </Svg>
  );
}

export function DispatchIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M14 2 7 9" />
      <path d="M14 2 9.5 14l-2.5-5-5-2.5L14 2Z" />
    </Svg>
  );
}

export function SettingsIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M2.5 4.5h6M11.5 4.5h2M2.5 11.5h2M7.5 11.5h6" />
      <circle cx="10" cy="4.5" r="1.5" />
      <circle cx="6" cy="11.5" r="1.5" />
    </Svg>
  );
}

export function ServerIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="2.5" y="2.5" width="11" height="8" rx="1.2" />
      <path d="M5.5 13.5h5M8 10.5v3" />
    </Svg>
  );
}

export function CpuIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="4" y="4" width="8" height="8" rx="1" />
      <path d="M6.5 1.5V4M9.5 1.5V4M6.5 12v2.5M9.5 12v2.5M1.5 6.5H4M1.5 9.5H4M12 6.5h2.5M12 9.5h2.5" />
    </Svg>
  );
}

export function BoxIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M8 1.8 13.5 4.8v6.4L8 14.2 2.5 11.2V4.8L8 1.8Z" />
      <path d="M2.5 4.8 8 7.8l5.5-3M8 7.8v6.4" />
    </Svg>
  );
}

export function NodeIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="8" cy="8" r="2" />
      <circle cx="3" cy="3.5" r="1.3" />
      <circle cx="13" cy="3.5" r="1.3" />
      <circle cx="8" cy="14" r="1.1" />
      <path d="M4 4.4 6.5 6.8M12 4.4 9.5 6.8M8 10v2.9" />
    </Svg>
  );
}

export function GaugeIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M2.5 11.5a5.5 5.5 0 1 1 11 0" />
      <path d="M8 11.5 10.5 7" />
    </Svg>
  );
}

export function TrashIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M2.5 5h11M6 5V3.5h4V5M3.5 5l.6 8.2a1 1 0 0 0 1 .8h5.8a1 1 0 0 0 1-.8L12.5 5" />
      <path d="M6.5 7.5v4M9.5 7.5v4" />
    </Svg>
  );
}

export function SearchIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <circle cx="7" cy="7" r="4.5" />
      <path d="m10.5 10.5 3 3" />
    </Svg>
  );
}

export function ChevronLeftIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M10 3.5 5.5 8l4.5 4.5" />
    </Svg>
  );
}

export function PlusIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M8 3v10M3 8h10" />
    </Svg>
  );
}

export function XIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="m4 4 8 8M12 4l-8 8" />
    </Svg>
  );
}

export function LockIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <rect x="3" y="7" width="10" height="7" rx="1.3" />
      <path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" />
    </Svg>
  );
}

export function LinkIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M6.5 9.5a3 3 0 0 0 4.2 0l2-2a3 3 0 0 0-4.2-4.2l-.6.6" />
      <path d="M9.5 6.5a3 3 0 0 0-4.2 0l-2 2a3 3 0 0 0 4.2 4.2l.6-.6" />
    </Svg>
  );
}

export function DownloadIcon(props: IconProps) {
  return (
    <Svg {...props}>
      <path d="M8 2.5v8M4.5 7 8 10.5 11.5 7M3 13.5h10" />
    </Svg>
  );
}

// A few line icons in the Phosphor style Argos uses, inlined so the UI needs nothing from a CDN.
import type { SVGProps } from "react";

function Svg(props: SVGProps<SVGSVGElement>) {
  return (
    <svg
      viewBox="0 0 16 16"
      width="13"
      height="13"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.3"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...props}
    />
  );
}

export const MapIcon = () => (
  <Svg>
    <circle cx="3.5" cy="4" r="1.6" />
    <circle cx="3.5" cy="12" r="1.6" />
    <circle cx="12.5" cy="8" r="1.6" />
    <path d="M5 4.6 11 7.4M5 11.4 11 8.6" />
  </Svg>
);
export const ListIcon = () => (
  <Svg>
    <path d="M5.5 4h8M5.5 8h8M5.5 12h8" />
    <path d="M2.5 4h.01M2.5 8h.01M2.5 12h.01" strokeWidth="2" />
  </Svg>
);
export const SendIcon = () => (
  <Svg>
    <path d="M14 2 7 9M14 2l-4.5 12L7 9 2 6.5 14 2Z" />
  </Svg>
);
export const GearIcon = () => (
  <Svg>
    <circle cx="8" cy="8" r="2.2" />
    <path d="M8 1.8v1.6M8 12.6v1.6M14.2 8h-1.6M3.4 8H1.8M12.4 3.6l-1.1 1.1M4.7 11.3l-1.1 1.1M12.4 12.4l-1.1-1.1M4.7 4.7 3.6 3.6" />
  </Svg>
);
export const RefreshIcon = () => (
  <Svg>
    <path d="M13.5 3v3.2h-3.2" />
    <path d="M2.5 13V9.8h3.2" />
    <path d="M12.8 6.2A5.2 5.2 0 0 0 3.6 5M3.2 9.8a5.2 5.2 0 0 0 9.2 1.2" />
  </Svg>
);

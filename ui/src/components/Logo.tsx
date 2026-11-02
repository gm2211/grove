/**
 * grove's mark: three trees on one line (a fleet of Macs on one tailnet), drawn as a ring like
 * Argos's crescent so the two products read as siblings.
 */
export function Logo() {
  return (
    <span className="flex items-center gap-2.5" aria-label="grove">
      <svg viewBox="0 0 28 28" width="24" height="24" fill="none" aria-hidden="true">
        <circle cx="14" cy="14" r="12.5" stroke="#3d4a51" strokeWidth="1.2" />
        <path d="M14 6.5 18.2 13.5H9.8Z" fill="#7ee7c7" />
        <path d="M8.6 10.5 11.6 15.6H5.6Z" fill="#7ee7c7" opacity="0.55" />
        <path d="M19.4 10.5 22.4 15.6H16.4Z" fill="#7ee7c7" opacity="0.55" />
        <path d="M14 13.5v6M8.6 15.6v3.9M19.4 15.6v3.9M5.5 19.5h17" stroke="#c3d1d5" strokeWidth="1.2" strokeLinecap="round" />
      </svg>
      <span className="mono hidden text-[15px] sm:inline font-medium tracking-[0.32em] text-[#eaf4f4]">GROVE</span>
    </span>
  );
}

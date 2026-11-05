export function Brand() {
  return (
    <span className="flex items-center gap-2.5">
      <svg viewBox="0 0 28 28" className="size-7 text-accent" fill="none" aria-hidden="true">
        <rect x="2" y="2" width="24" height="24" rx="6" fill="currentColor" />
        <path d="M8 9h12M14 9v11" stroke="white" strokeWidth="2.5" />
      </svg>
      <span className="text-[17px] font-semibold tracking-[-0.04em]">teras data</span>
    </span>
  );
}

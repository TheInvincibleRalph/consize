export function Brand({ size = "md" }: { size?: "md" | "lg" }) {
  const mark = size === "lg" ? 36 : 26;
  const word = size === "lg" ? "text-[24px]" : "text-[16px]";

  return (
    <div className="brand-lockup flex items-center gap-2.5" aria-label="conSize">
      <svg width={mark} height={mark} viewBox="0 0 32 32" className="brand-mark" aria-hidden>
        <rect className="brand-mark-frame" x="2" y="2" width="28" height="28" rx="8" />
        <path className="brand-mark-cut brand-mark-cut-top" d="M23 2h7v7" />
        <path className="brand-mark-cut brand-mark-cut-bottom" d="M2 23v7h7" />
        <path
          className="brand-mark-glyph"
          d="M21.2 9.7h-6.8c-2 0-3.5 1.15-3.5 2.85 0 1.85 1.35 2.5 3.45 2.9l3.35.62c2.2.42 3.55 1.25 3.55 3.25 0 1.78-1.55 2.98-3.72 2.98H10.8"
        />
      </svg>
      <span className={`brand-name ${word}`} aria-hidden>
        <span className="brand-name-con">con</span>
        <span className="brand-name-size">
          <span className="brand-name-s">S</span>ize
        </span>
      </span>
    </div>
  );
}

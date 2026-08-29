"use client";

import { sliSummary } from "@/lib/format";

type SliSignal = Record<string, unknown>;

const SIGNAL_LABELS: Record<string, string> = {
  evictions: "Evictions",
  oom_killed: "OOM kills",
  restarts: "Restarts",
  throttling: "Throttling",
};

const VERDICT_RANK: Record<string, number> = {
  failed: 0,
  inconclusive: 1,
  unavailable: 2,
  passed: 3,
};

function fmtValue(v: unknown): string {
  if (typeof v !== "number") return String(v ?? "");
  if (!Number.isFinite(v)) return "—";
  if (Number.isInteger(v)) return String(v);
  return v.toLocaleString("en-US", { maximumFractionDigits: 1 });
}

function shortReason(reason: unknown): string | null {
  if (typeof reason !== "string" || !reason.trim()) return null;
  if (reason.includes("data missing")) return "data missing";
  if (reason.includes("new events")) return "new events";
  return reason.length > 28 ? `${reason.slice(0, 25)}…` : reason;
}

function signalRows(slis: unknown) {
  if (!slis || typeof slis !== "object") return [];
  return Object.entries(slis as Record<string, unknown>)
    .map(([key, value]) => {
      const signal = value && typeof value === "object" ? (value as SliSignal) : {};
      const verdict = String(signal.verdict ?? value ?? "unknown");
      const baseline = signal.baseline_total;
      const post = signal.post_total;
      const reason = shortReason(signal.reason);
      let detail = reason;

      if (!detail && typeof baseline === "number" && typeof post === "number") {
        detail = baseline === 0 && post === 0 ? "0 events" : `${fmtValue(baseline)} → ${fmtValue(post)}`;
      }
      if (!detail && verdict === "unavailable") detail = "not measured";

      return {
        key,
        label: SIGNAL_LABELS[key] ?? key.replaceAll("_", " "),
        verdict,
        detail,
      };
    })
    .sort((a, b) => {
      const ar = VERDICT_RANK[a.verdict] ?? 9;
      const br = VERDICT_RANK[b.verdict] ?? 9;
      return ar === br ? a.label.localeCompare(b.label) : ar - br;
    });
}

export function SliSignals({
  slis,
  compact = false,
}: {
  slis: unknown;
  compact?: boolean;
}) {
  const rows = signalRows(slis);
  if (!rows.length) return <span className="sli-empty">No SLI details</span>;

  return (
    <div className={`sli-signals ${compact ? "compact" : ""}`} title={sliSummary(slis)}>
      {rows.map((row) => (
        <span className={`sli-chip ${row.verdict}`} key={row.key}>
          <span className="sli-chip-name">{row.label}</span>
          <span className="sli-chip-verdict">{row.verdict}</span>
          {row.detail ? <span className="sli-chip-detail">{row.detail}</span> : null}
        </span>
      ))}
    </div>
  );
}

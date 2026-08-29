const MiB = 1024 * 1024;

export function money(n: unknown): string {
  const v = Number(n);
  if (Number.isNaN(v)) return "—";
  return "$" + v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

export function pct(n: unknown): string {
  const v = Number(n);
  return Number.isNaN(v) ? "—" : Math.round(v * 100) + "%";
}

export function fmtBytes(n: unknown): string {
  const m = Number(n) / MiB;
  if (Number.isNaN(m)) return "—";
  if (m >= 1024) return (m / 1024).toFixed(2) + " GiB";
  return (m >= 100 ? m.toFixed(0) : m.toFixed(1)) + " MiB";
}

export function fmtMilli(n: unknown): string {
  const v = Number(n);
  if (Number.isNaN(v)) return "—";
  return v >= 1000 ? (v / 1000).toFixed(2) + " cores" : v + "m";
}

export function currentOf(r: { Resource?: string; CurrentValue?: number }): string {
  return r.Resource === "memory" ? fmtBytes(r.CurrentValue) : fmtMilli(r.CurrentValue);
}
export function proposedOf(r: { Resource?: string; ProposedValue?: number }): string {
  return r.Resource === "memory" ? fmtBytes(r.ProposedValue) : fmtMilli(r.ProposedValue);
}

export function fmtTime(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString();
}

export function fmtNum(v: unknown): string {
  if (typeof v !== "number") return String(v ?? "");
  return Number.isInteger(v) ? String(v) : String(Math.round(v * 100) / 100);
}

export function shortClass(name: string | undefined | null): string {
  return name ? (String(name).split(".").pop() ?? "") : String(name ?? "");
}

export function sliSummary(slis: unknown): string {
  if (!slis || typeof slis !== "object") return "—";
  const parts = Object.entries(slis as Record<string, unknown>).map(([k, v]) => {
    if (v && typeof v === "object") {
      const inner = Object.entries(v as Record<string, unknown>)
        .map(([ik, iv]) => `${ik}=${fmtNum(iv)}`)
        .join(", ");
      return `${k}={${inner}}`;
    }
    return `${k}=${fmtNum(v)}`;
  });
  return parts.length ? parts.join(" · ") : "—";
}

export function diffLine(d: { resource?: string; current_request?: number; proposed_request?: number; current_class?: string; proposed_class?: string } | null | undefined): string {
  if (!d?.resource) return "—";
  if (d.resource === "class") return `${shortClass(d.current_class)} → ${shortClass(d.proposed_class)}`;
  if (d.resource === "memory") return `${fmtBytes(d.current_request)} → ${fmtBytes(d.proposed_request)}`;
  if (d.resource === "cpu") return `${fmtMilli(d.current_request)} → ${fmtMilli(d.proposed_request)}`;
  return "—";
}

export function isDBWorkload(w: { Source?: string; Kind?: string }): boolean {
  return w.Source === "db" || w.Kind === "database";
}

export function isDBRecommendation(r: { Resource?: string }): boolean {
  return r.Resource === "class";
}

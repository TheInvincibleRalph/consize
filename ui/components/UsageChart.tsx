"use client";

import React from "react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { TooltipContentProps } from "recharts";
import { api } from "@/lib/api";
import type { SeriesResponse } from "@/lib/types";

interface SeriesPoint {
  ts: number;
  p50?: number;
  p95?: number;
  p99?: number;
  max?: number;
}

const SERIES = [
  { key: "p50", label: "p50", color: "#3987e5" },
  { key: "p95", label: "p95", color: "#d95926" },
  { key: "p99", label: "p99", color: "#199e70" },
  { key: "max", label: "max", color: "#c98500" },
] as const;

interface MetricDef {
  key: string;
  label: string;
  pct: boolean; // percent metrics pin the Y axis to 0..100
  unit: string; // expected series unit for tooltip/labels ("percent", "millicores", "bytes", "iops", "connections")
}

const DB_METRICS: MetricDef[] = [
  { key: "cpu_percent", label: "CPU %", pct: true, unit: "percent" },
  { key: "mem_percent", label: "Memory %", pct: true, unit: "percent" },
  { key: "iops", label: "IOPS", pct: false, unit: "iops" },
  { key: "connections", label: "Connections", pct: false, unit: "connections" },
];

const COMPUTE_METRICS: MetricDef[] = [
  { key: "cpu_percent", label: "CPU (millicores)", pct: false, unit: "millicores" },
  { key: "mem_percent", label: "Memory (bytes)", pct: false, unit: "bytes" },
];

const MiB = 1024 * 1024;

function niceMax(v: number): number {
  if (!(v > 0)) return 100;
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / mag;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
  return step * mag;
}

function fmtValue(v: number, unit: string | undefined, pct: boolean): string {
  if (unit === "percent") return Math.round(v) + "%";
  if (unit === "millicores") return v >= 1000 ? (v / 1000).toFixed(2) + " cores" : Math.round(v) + "m";
  if (unit === "bytes") {
    const m = v / MiB;
    if (m >= 1024) return (m / 1024).toFixed(1) + " GiB";
    return (m >= 100 ? m.toFixed(0) : m.toFixed(1)) + " MiB";
  }
  if (pct) return Math.round(v) + "%";
  return Math.round(v).toLocaleString("en-US");
}

/* Normalize several plausible series response shapes. */
function normalizeSeries(body: SeriesResponse | null | undefined): SeriesPoint[] {
  if (!body || typeof body !== "object") return [];
  const o = body as unknown as Record<string, unknown>;
  const arr = Array.isArray(body)
    ? body
    : (o.points ?? o.series ?? o.data ?? o.buckets ?? o.rows ?? o.values);
  if (!Array.isArray(arr)) return [];
  const out: SeriesPoint[] = [];
  arr.forEach((pt: unknown) => {
    if (!pt || typeof pt !== "object") return;
    const o = pt as Record<string, unknown>;
    const tsRaw = o.ts ?? o.t ?? o.time ?? o.timestamp ?? o.window_start ?? o.windowStart ?? o.date;
    const ms = typeof tsRaw === "number" ? tsRaw : Date.parse(String(tsRaw));
    if (Number.isNaN(ms)) return;
    out.push({
      ts: ms,
      p50: num(o, ["p50", "p50th", "percentile_50", "P50"]),
      p95: num(o, ["p95", "p95th", "percentile_95", "P95"]),
      p99: num(o, ["p99", "p99th", "percentile_99", "P99"]),
      max: num(o, ["max", "maximum", "Max"]),
    });
  });
  out.sort((a, b) => a.ts - b.ts);
  return out;
}

function num(o: Record<string, unknown>, keys: string[]): number | undefined {
  for (const k of keys) {
    const v = o[k];
    if (typeof v === "number" && !Number.isNaN(v)) return v;
    if (typeof v === "string" && v !== "" && !Number.isNaN(Number(v))) return Number(v);
  }
  return undefined;
}

const AXIS_TICK = { fill: "#8b96a8", fontSize: 10.5 };
const GRID_STROKE = "rgba(148, 163, 184, 0.08)";

export function UsageChart({
  workloadId,
  isDB,
}: {
  workloadId: string | number;
  isDB: boolean;
}) {
  const metrics = isDB ? DB_METRICS : COMPUTE_METRICS;
  const [metric, setMetric] = React.useState<MetricDef>(metrics[0]);
  const [points, setPoints] = React.useState<SeriesPoint[]>([]);
  const [unit, setUnit] = React.useState<string | undefined>(undefined);
  const [status, setStatus] = React.useState<"loading" | "ready" | "empty">("loading");
  const requestId = React.useRef(0);

  React.useEffect(() => {
    const id = ++requestId.current;
    let cancelled = false;
    Promise.resolve()
      .then(() => {
        if (cancelled || id !== requestId.current) return null;
        setStatus("loading");
        return api.series(workloadId, { metric: metric.key, days: 14 });
      })
      .then((body) => {
        if (!body || cancelled || id !== requestId.current) return;
        const pts = normalizeSeries(body);
        setUnit(body?.unit);
        if (!pts.length) {
          setPoints([]);
          setStatus("empty");
        } else {
          setPoints(pts);
          setStatus("ready");
        }
      })
      .catch(() => {
        if (cancelled || id !== requestId.current) return;
        setPoints([]);
        setStatus("empty");
      });
    return () => {
      cancelled = true;
    };
  }, [metric, workloadId]);

  // Y domain: percent metrics pin to 0..100; otherwise a nice ceiling.
  const dataMax = points.reduce((m, p) => {
    SERIES.forEach((s) => {
      const v = p[s.key];
      if (typeof v === "number" && v > m) m = v;
    });
    return m;
  }, 0);
  const yDomain: [number, number] = metric.pct ? [0, 100] : [0, niceMax(dataMax)];

  const yTick = (v: number) => fmtValue(v, unit, metric.pct);
  const xTick = (ts: number) =>
    new Date(ts).toLocaleDateString("en-US", { month: "short", day: "numeric" });

  function tooltipBody({ active, payload, label }: TooltipContentProps<number, string>) {
    if (!active || !payload || !payload.length) return null;
    return (
      <div className="rounded-lg border border-edge2 bg-panel2 px-3 py-2 shadow-xl">
        <div className="micro mb-1.5">
          {new Date(Number(label)).toLocaleString("en-US", {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit",
          })}
        </div>
        {payload.map((p) => (
          <div key={String(p.dataKey)} className="flex items-center gap-2 py-0.5 text-[12px] text-ink2">
            <span
              className="h-[3px] w-[14px] rounded-sm"
              style={{ background: p.color || "#8b96a8" }}
            />
            <span className="text-muted">{String(p.dataKey)}</span>
            <span className="mono ml-auto pl-4 text-ink">
              {typeof p.value === "number" ? fmtValue(p.value, unit, metric.pct) : "—"}
            </span>
          </div>
        ))}
      </div>
    );
  }

  return (
    <div className="card chart-card">
      <div className="card-head">
        <h2 className="card-title">Usage — last 14 days</h2>
      </div>
      <div className="chart-toolbar">
        <div className="chart-legend">
          {SERIES.map((s) => (
            <span key={s.key} className="key">
              <span className="sw" style={{ background: s.color }} />
              {s.label}
            </span>
          ))}
        </div>
        <div className="seg-row">
          {metrics.map((m) => (
            <button
              key={m.key}
              className={`seg ${m.key === metric.key ? "active" : ""}`}
              onClick={() => setMetric(m)}
              type="button"
            >
              {m.label}
            </button>
          ))}
        </div>
      </div>

      <div className="px-4 pb-4 pt-3" style={{ height: 300 }}>
        {status === "loading" && (
          <div className="chart-empty">
            <div className="spinner" />
            <strong>Loading usage data…</strong>
          </div>
        )}
        {status === "empty" && (
          <div className="chart-empty">
            <strong>No chart data yet</strong>
            <span>The series endpoint has no data for this workload and metric yet.</span>
          </div>
        )}
        {status === "ready" && (
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={points} margin={{ top: 6, right: 8, bottom: 2, left: 0 }}>
              <CartesianGrid stroke={GRID_STROKE} strokeDasharray="0" vertical={false} />
              <XAxis
                dataKey="ts"
                type="number"
                domain={["dataMin", "dataMax"]}
                scale="time"
                tick={AXIS_TICK}
                tickFormatter={xTick}
                tickMargin={8}
                minTickGap={28}
                axisLine={{ stroke: "rgba(148, 163, 184, 0.25)" }}
                tickLine={false}
              />
              <YAxis
                domain={yDomain}
                tick={AXIS_TICK}
                tickFormatter={yTick}
                tickMargin={6}
                width={54}
                axisLine={false}
                tickLine={false}
              />
              <Tooltip
                content={(props) => tooltipBody(props as unknown as TooltipContentProps<number, string>)}
                cursor={{ stroke: "rgba(148, 163, 184, 0.28)", strokeWidth: 1 }}
              />
              {SERIES.map((s) => (
                <Line
                  key={s.key}
                  type="monotone"
                  dataKey={s.key}
                  stroke={s.color}
                  strokeWidth={2}
                  dot={points.length === 1 ? { r: 4, fill: s.color, strokeWidth: 0 } : false}
                  activeDot={{ r: 4, strokeWidth: 0 }}
                  connectNulls
                  isAnimationActive={false}
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        )}
      </div>
    </div>
  );
}

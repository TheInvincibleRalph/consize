"use client";

import Link from "next/link";
import React from "react";
import type { LucideIcon } from "lucide-react";
import {
  Activity,
  ArrowRight,
  BadgeDollarSign,
  Boxes,
  ListChecks,
  RefreshCw,
  ShieldCheck,
} from "lucide-react";
import { api, ApiError, pick } from "@/lib/api";
import type {
  ApplyEvent,
  Recommendation,
  SavingsResponse,
  SeriesResponse,
  SystemStatusResponse,
  VerificationRun,
  Workload,
} from "@/lib/types";
import { fmtTime, money } from "@/lib/format";
import { SliSignals } from "@/components/SliSignals";
import {
  EmptyState,
  ErrorState,
  LoadingState,
  resultPill,
  verdictPill,
} from "@/components/ui";
import { ApplyModal } from "@/components/ApplyModal";
import { continuationFor } from "@/components/ApplyTimeline";
import { useAuth } from "@/components/auth";

type DashboardReadyState = {
  phase: "ready";
  workloads: Workload[];
  pendingRecommendations: Recommendation[];
  applies: ApplyEvent[];
  runs: VerificationRun[];
  savings: SavingsResponse;
  system: SystemStatusResponse | null;
  updatedAt: Date;
};

function KpiTile({
  label,
  value,
  icon: Icon,
  tone = "neutral",
}: {
  label: string;
  value: string;
  icon: LucideIcon;
  tone?: "neutral" | "money" | "warning";
}) {
  return (
    <div className={`kpi-card ${tone}`}>
      <div className="kpi-icon" aria-hidden="true">
        <Icon size={18} strokeWidth={1.8} />
      </div>
      <div>
        <div className="kpi-label">{label}</div>
        <div className="kpi-value">{value}</div>
      </div>
    </div>
  );
}

function durationLabel(seconds: number | null | undefined): string {
  if (seconds == null) return "—";
  if (seconds < 60) return `${Math.max(0, Math.round(seconds))}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  return `${hours}h`;
}

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

function pipelineTitle(status: SystemStatusResponse): string {
  if (status.verification_due > 0) return "Safety check ready";
  if (status.in_flight_applies > 0) return "Safety check scheduled";
  if (status.status === "healthy") return "System healthy";
  if (status.status === "empty") return "Waiting for first collection";
  if (status.status === "unavailable") return "Consize unavailable";
  return "Telemetry may be stale";
}

function safetyMessage(status: SystemStatusResponse, latest: string): string {
  if (status.verification_due > 0) {
    return `${plural(status.verification_due, "applied change")} ready for verification. Consize will run the safety check automatically within about a minute.`;
  }
  if (status.in_flight_applies > 0) {
    const nextDue = status.next_verification_due_at
      ? ` Next verification opens at ${fmtTime(status.next_verification_due_at)}.`
      : "";
    return `${plural(status.in_flight_applies, "applied change")} collecting post-change telemetry before verification.${nextDue}`;
  }
  if (status.status === "healthy") {
    return "Telemetry is fresh and all applied changes have completed safety checks.";
  }
  return status.messages?.[0] ?? `Latest telemetry bucket: ${latest}`;
}

function safetyMetric(status: SystemStatusResponse): string {
  if (status.verification_due > 0) return `Safety checks: ${status.verification_due} due`;
  if (status.in_flight_applies > 0) return `Safety checks: ${status.in_flight_applies} scheduled`;
  return "Safety checks: clear";
}

function nextVerificationMetric(status: SystemStatusResponse): string | null {
  if (!status.next_verification_due_at || status.verification_due > 0) return null;
  return `Next check: ${fmtTime(status.next_verification_due_at)}`;
}

function SystemStatusCard({ status }: { status: SystemStatusResponse }) {
  const tone = status.status === "healthy" ? "healthy" : status.status === "empty" ? "empty" : "degraded";
  const latest = status.latest_usage_bucket ? fmtTime(status.latest_usage_bucket) : "no telemetry yet";
  const message = safetyMessage(status, latest);
  const nextCheck = nextVerificationMetric(status);

  return (
    <div className={`system-status ${tone}`}>
      <div>
        <div className="system-status-title">
          <span className="system-status-dot" />
          {pipelineTitle(status)}
        </div>
        <p>{message}</p>
      </div>
      <div className="system-status-metrics">
        <span>Latest telemetry: {latest}</span>
        <span>Age: {durationLabel(status.telemetry_age_seconds)}</span>
        {nextCheck && <span>{nextCheck}</span>}
        <span>{safetyMetric(status)}</span>
      </div>
    </div>
  );
}

function RecentApplyCard({
  applies,
  runsByEvent,
  pendingRecommendations,
  canApply,
  nameOf,
  onContinue,
}: {
  applies: ApplyEvent[];
  runsByEvent: Map<number, VerificationRun>;
  pendingRecommendations: Recommendation[];
  canApply: boolean;
  nameOf: (id: number) => string;
  onContinue: (rec: Recommendation) => void;
}) {
  return (
    <section className="card activity-card">
      <div className="card-head">
        <div>
          <h2 className="card-title">Recent changes</h2>
        </div>
        <Link className="card-link" href="/audit">
          Audit <ArrowRight size={14} />
        </Link>
      </div>
      {applies.length === 0 ? (
        <EmptyState msg="No apply events yet." />
      ) : (
        <div className="activity-list">
          {applies.slice(0, 6).map((event) => {
            const run = runsByEvent.get(event.ID);
            const hasMoreSteps = event.Result === "applied" && event.StepNumber > 0 && event.TotalSteps > event.StepNumber;
            const continuation =
              run?.Verdict === "passed" && hasMoreSteps
                ? continuationFor(event, pendingRecommendations)
                : null;
            return (
              <article className="activity-row" key={event.ID}>
                <div className={`activity-icon ${event.Result}`}>
                  <Activity size={16} />
                </div>
                <div className="activity-main">
                  <div className="activity-title">
                    <span>{nameOf(event.WorkloadID)}</span>
                    {resultPill(event.Result)}
                  </div>
                  <div className="activity-meta">
                    <span>Step {event.StepNumber}/{event.TotalSteps}</span>
                    <span>{event.Actor}</span>
                    <span>{fmtTime(event.CreatedAt)}</span>
                  </div>
                </div>
                <div className="activity-step">
                  {continuation ? (
                    <button
                      className="btn small continue-step"
                      type="button"
                      onClick={() => onContinue(continuation)}
                      disabled={!canApply}
                    >
                      Continue step {event.StepNumber + 1}/{event.TotalSteps}
                    </button>
                  ) : (
                    <span>{event.StepNumber}/{event.TotalSteps}</span>
                  )}
                </div>
              </article>
            );
          })}
        </div>
      )}
    </section>
  );
}

function VerificationCard({ runs }: { runs: VerificationRun[] }) {
  return (
    <section className="card activity-card">
      <div className="card-head">
        <div>
          <h2 className="card-title">Verification</h2>
        </div>
        <Link className="card-link" href="/audit">
          Audit <ArrowRight size={14} />
        </Link>
      </div>
      {runs.length === 0 ? (
        <EmptyState msg="No verification runs yet." />
      ) : (
        <div className="verification-list dashboard-verification-list">
          {runs.slice(0, 5).map((run) => (
            <article className="verification-item" key={run.ID}>
              <div className="verification-item-top">
                <div className="verification-ids">
                  <span className="mono text-faint">run #{run.ID}</span>
                  <span className="verification-sep">/</span>
                  <span className="mono text-faint">apply #{run.ApplyEventID}</span>
                </div>
                <div className="verification-meta">
                  {verdictPill(run.Verdict)}
                  <span className="text-faint">{fmtTime(run.CreatedAt)}</span>
                </div>
              </div>
              <SliSignals slis={run.SLIs} />
            </article>
          ))}
        </div>
      )}
    </section>
  );
}

function pointTime(raw: unknown): number | null {
  if (!raw || typeof raw !== "object") return null;
  const o = raw as Record<string, unknown>;
  const tsRaw = o.ts ?? o.t ?? o.time ?? o.timestamp ?? o.window_start ?? o.windowStart ?? o.date;
  if (typeof tsRaw === "number") {
    const ms = tsRaw < 1_000_000_000_000 ? tsRaw * 1000 : tsRaw;
    return Number.isFinite(ms) ? ms : null;
  }
  if (typeof tsRaw === "string" && tsRaw.trim() !== "") {
    const ms = Date.parse(tsRaw);
    return Number.isNaN(ms) ? null : ms;
  }
  return null;
}

function latestSeriesTime(body: SeriesResponse | null | undefined): number | null {
  if (!body || typeof body !== "object") return null;
  const o = body as unknown as Record<string, unknown>;
  const arr = Array.isArray(body)
    ? body
    : (o.points ?? o.series ?? o.data ?? o.buckets ?? o.rows ?? o.values);
  if (!Array.isArray(arr)) return null;
  return arr.reduce<number | null>((latest, pt) => {
    const ms = pointTime(pt);
    if (ms == null) return latest;
    return latest == null || ms > latest ? ms : latest;
  }, null);
}

function latestTodayOrYesterdayLabel(latestMs: number | null): string {
  if (latestMs == null) return "No chart telemetry found";
  const latest = new Date(latestMs);
  const now = new Date();
  const latestDay = new Date(latest.getFullYear(), latest.getMonth(), latest.getDate()).getTime();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  if (latestDay === today) return "Latest sampled telemetry is from today";
  if (today - latestDay === 24 * 60 * 60 * 1000) return "Latest sampled telemetry is from yesterday";
  return `Latest sampled telemetry is from ${latest.toLocaleDateString("en-US", {
    month: "short",
    day: "numeric",
  })}`;
}

async function fallbackSystemStatus({
  workloads,
  applies,
  runs,
  savings,
}: {
  workloads: Workload[];
  applies: ApplyEvent[];
  runs: VerificationRun[];
  savings: SavingsResponse;
}): Promise<SystemStatusResponse> {
  const generatedAt = new Date();
  const pending = pick<number>(savings, [
    "active_recommendations",
    "pending_recommendations",
    "pending_count",
  ]);
  const verifiedApplyIDs = new Set(runs.map((run) => run.ApplyEventID));
  const inFlightApplies = applies.filter(
    (event) => event.Result === "applied" && !verifiedApplyIDs.has(event.ID),
  );
  const verifyWindowSeconds = 60 * 60;
  const verificationDue = inFlightApplies.filter((event) => {
    const created = Date.parse(event.CreatedAt);
    const step = Math.max(1, event.StepNumber || 1);
    return !Number.isNaN(created) && generatedAt.getTime() - created >= verifyWindowSeconds * step * 1000;
  }).length;

  const sampled = workloads.slice(0, 6);
  const sampledSeries = await Promise.allSettled(
    sampled.map((workload) => api.series(workload.ID, { metric: "cpu_percent", days: 3 })),
  );
  const latestMs = sampledSeries.reduce<number | null>((latest, result) => {
    if (result.status !== "fulfilled") return latest;
    const ms = latestSeriesTime(result.value);
    if (ms == null) return latest;
    return latest == null || ms > latest ? ms : latest;
  }, null);

  const staleAfterSeconds = 36 * 60 * 60;
  const telemetryAgeSeconds =
    latestMs == null ? null : Math.max(0, Math.round((generatedAt.getTime() - latestMs) / 1000));
  const latestISO = latestMs == null ? null : new Date(latestMs).toISOString();
  const messages: string[] = [];
  let status: SystemStatusResponse["status"] = "healthy";

  if (workloads.length === 0) {
    status = "empty";
    messages.push("No workloads have been collected yet.");
  } else if (latestMs == null) {
    status = "degraded";
    messages.push("No recent workload telemetry was found in the sampled chart data.");
  } else if (telemetryAgeSeconds != null && telemetryAgeSeconds > staleAfterSeconds) {
    status = "degraded";
    messages.push(`${latestTodayOrYesterdayLabel(latestMs)}. The collector may not be feeding new data.`);
  } else if (verificationDue > 0) {
    status = "attention";
    messages.push(`${plural(verificationDue, "applied change")} ready for automatic safety verification.`);
  } else {
    messages.push(`${latestTodayOrYesterdayLabel(latestMs)} across sampled workloads.`);
  }

  messages.push("Using sampled daily chart data until the live API exposes precise system status.");

  return {
    status,
    generated_at: generatedAt.toISOString(),
    latest_usage_bucket: latestISO,
    telemetry_age_seconds: telemetryAgeSeconds,
    stale_after_seconds: staleAfterSeconds,
    verify_window_seconds: verifyWindowSeconds,
    workloads: workloads.length,
    pending_recommendations: pending ?? 0,
    in_flight_applies: inFlightApplies.length,
    verification_due: verificationDue,
    next_verification_due_at: null,
    store: "api-series-fallback",
    messages,
  };
}

export default function DashboardView() {
  const { authEnabled, user } = useAuth();
  const canApply = !authEnabled || user?.role === "operator" || user?.role === "admin";
  const [state, setState] = React.useState<
    | { phase: "loading" }
    | { phase: "refreshing"; previous: DashboardReadyState }
    | { phase: "error"; msg: string }
    | DashboardReadyState
  >({ phase: "loading" });
  const [applyRec, setApplyRec] = React.useState<Recommendation | null>(null);

  const fetchDashboard = React.useCallback(async (): Promise<DashboardReadyState> => {
    const [savings, workloads, applies, runs, recBody] = await Promise.all([
      api.savings(),
      api.workloads(),
      api.applies(),
      api.verificationRuns(),
      api.recommendations({ status: "pending", limit: 100 }),
    ]);
    const system =
      (await api.systemStatus().catch(() =>
        fallbackSystemStatus({ workloads, applies, runs, savings: savings || {} }),
      )) ?? null;

    return {
      phase: "ready",
      workloads,
      pendingRecommendations: recBody.recommendations || [],
      applies,
      runs,
      savings: savings || {},
      system,
      updatedAt: new Date(),
    };
  }, []);

  React.useEffect(() => {
    let cancelled = false;
    fetchDashboard()
      .then((next) => {
        if (!cancelled) setState(next);
      })
      .catch((err: unknown) => {
        if (!cancelled) setState({ phase: "error", msg: errMessage(err) });
      });
    return () => {
      cancelled = true;
    };
  }, [fetchDashboard]);

  const refresh = React.useCallback(() => {
    setState((current) =>
      current.phase === "ready"
        ? { phase: "refreshing", previous: current }
        : current.phase === "refreshing"
          ? current
          : { phase: "loading" },
    );
    fetchDashboard()
      .then((next) => {
        setState(next);
      })
      .catch((err: unknown) => {
        setState({ phase: "error", msg: errMessage(err) });
      });
  }, [fetchDashboard]);

  if (state.phase === "loading") {
    return <LoadingState msg="Loading dashboard…" />;
  }
  if (state.phase === "error") {
    return <ErrorState msg={state.msg} />;
  }

  const view = state.phase === "refreshing" ? state.previous : state;
  const refreshing = state.phase === "refreshing";
  const { workloads, pendingRecommendations, applies, runs, savings, system, updatedAt } = view;
  const runsByEvent = new Map(runs.map((run) => [run.ApplyEventID, run]));

  const projected = pick<number>(savings, [
    "projected_monthly_savings",
    "projected_monthly",
    "projected",
  ]);
  const realized = pick<number>(savings, [
    "realized_monthly_savings",
    "realized_monthly",
    "realized_savings_monthly",
  ]);
  const active = pick<number>(savings, [
    "active_recommendations",
    "pending_recommendations",
    "pending_count",
  ]);

  const nameOf = (id: number) => {
    const w = workloads.find((x) => x.ID === id);
    return w ? `${w.Namespace}/${w.Name}` : `workload #${id}`;
  };

  const projectedValue = projected ?? 0;
  const realizedValue = realized ?? 0;

  return (
    <div className="dashboard">
      <section className="dashboard-hero">
        <div className="dashboard-hero-main">
          <div>
            <p className="dashboard-eyebrow">Overview</p>
            <h1 className="dashboard-title">Dashboard</h1>
          </div>
          <div className="dashboard-actions">
            <span className="dashboard-updated">Updated {fmtTime(updatedAt.toISOString())}</span>
            <button className="btn small" type="button" onClick={refresh} disabled={refreshing}>
              <RefreshCw size={14} className={refreshing ? "spin" : ""} />
              Refresh
            </button>
          </div>
        </div>

        {system ? <SystemStatusCard status={system} /> : null}
      </section>

      <div className="dashboard-kpis">
        <KpiTile
          icon={Boxes}
          label="Workloads"
          value={String(workloads.length)}
        />
        <KpiTile
          icon={ListChecks}
          label="Recommendations"
          value={active == null ? "—" : String(active)}
        />
        <KpiTile
          icon={BadgeDollarSign}
          label="Projected savings"
          value={money(projectedValue)}
          tone="money"
        />
        <KpiTile
          icon={ShieldCheck}
          label="Realized savings"
          value={money(realizedValue)}
          tone="money"
        />
      </div>

      <div className="dashboard-grid two">
        <RecentApplyCard
          applies={applies}
          runsByEvent={runsByEvent}
          pendingRecommendations={pendingRecommendations}
          canApply={canApply}
          nameOf={nameOf}
          onContinue={setApplyRec}
        />
        <VerificationCard runs={runs} />
      </div>

      {applyRec && (
        <ApplyModal
          rec={applyRec}
          onClose={() => setApplyRec(null)}
          onApplied={() => {
            setApplyRec(null);
            refresh();
          }}
        />
      )}
    </div>
  );
}

function errMessage(err: unknown): string {
  if (err instanceof ApiError) {
    return `${err.message}${err.body && typeof err.body === "object" && "error" in (err.body as object) ? `: ${(err.body as { error: string }).error}` : ""}`;
  }
  return String((err as Error)?.message || err);
}

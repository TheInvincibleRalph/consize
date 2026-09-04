"use client";

import Link from "next/link";
import React from "react";
import { api, ApiError, pick } from "@/lib/api";
import type { ApplyEvent, Recommendation, VerificationRun, Workload } from "@/lib/types";
import { fmtBytes, fmtMilli, fmtTime, isDBWorkload, money, pct } from "@/lib/format";
import {
  Avatar,
  Chip,
  Delta,
  ErrorState,
  LoadingState,
  NotFoundState,
  Table,
  kindPill,
  riskPill,
} from "@/components/ui";
import { UsageChart } from "@/components/UsageChart";
import { ApplyTimeline } from "@/components/ApplyTimeline";
import { ApplyModal } from "@/components/ApplyModal";
import { useAuth } from "@/components/auth";

interface DetailData {
  workload: Workload;
  recs: Recommendation[];
  applies: ApplyEvent[];
  runsByEvent: Map<number, VerificationRun>;
}

export default function WorkloadDetailView({ id }: { id: string }) {
  const { authEnabled, user } = useAuth();
  // Role gate : viewers see read views only; the server enforces
  
  const canApply = !authEnabled || user?.role === "operator" || user?.role === "admin";
  const [phase, setPhase] = React.useState<"loading" | "error" | "notfound" | "ready">("loading");
  const [data, setData] = React.useState<DetailData | null>(null);
  const [applyRec, setApplyRec] = React.useState<Recommendation | null>(null);
  const [reloadKey, setReloadKey] = React.useState(0);

  React.useEffect(() => {
    let alive = true;
    Promise.all([
      api.workload(id),
      api.recommendations({ workload_id: id, status: "pending", limit: 50 }),
      api.applies({ workload_id: id }),
      api.verificationRuns(),
    ])
      .then(([wl, recsBody, applies, runs]) => {
        if (!alive) return;
        const recs = recsBody?.recommendations || [];
        const runsByEvent = new Map<number, VerificationRun>();
        runs.forEach((r) => runsByEvent.set(r.ApplyEventID, r));
        setData({ workload: wl, recs, applies, runsByEvent });
        setPhase("ready");
      })
      .catch((err: unknown) => {
        if (!alive) return;
        if (err instanceof ApiError && err.status === 404) {
          setPhase("notfound");
        } else {
          setPhase("error");
        }
      });
    return () => {
      alive = false;
    };
  }, [id, reloadKey]);

  if (phase === "loading") return <LoadingState msg="Loading workload…" />;
  if (phase === "notfound") {
    return (
      <NotFoundState
        title="Workload not found"
        sub="It may have been removed from the managed surface."
        href="/workloads"
        linkLabel="Back to workloads"
      />
    );
  }
  if (phase === "error" || !data) return <ErrorState msg="Workload failed to load." />;

  const { workload: wl, recs, applies, runsByEvent } = data;
  const currentRecs = recs.filter((r) => r.Status === "pending");
  const isDB = isDBWorkload(wl);
  const risk = pick<string>(wl, ["Risk", "risk"]);
  const reasons = pick<string[] | string>(wl, ["RiskReasons", "risk_reasons"]);
  const curRows: [string, React.ReactNode][] = isDB
    ? [
        ["Instance class", <Chip key="c" cls="class">{wl.DBClass || "—"}</Chip>],
        ["Replicas", String(wl.DBReplicas ?? "—")],
        ["Maintenance window", wl.DBMaintenanceWindow || "—"],
        ["Provider", wl.DBProvider || "—"],
      ]
    : [
        ["CPU request", fmtMilli(wl.RequestCPUMilli)],
        ["CPU limit", fmtMilli(wl.LimitCPUMilli)],
        ["Memory request", fmtBytes(wl.RequestMemBytes)],
        ["Memory limit", fmtBytes(wl.LimitMemBytes)],
      ];

  return (
    <div>
      <Link className="back-link" href="/workloads">
        ← All workloads
      </Link>

      <section className="detail-hero">
        <Avatar name={wl.Name} kind={isDB ? "db" : "k8s"} large />
        <div className="detail-title-block">
          <p className="page-eyebrow">Workload</p>
          <h1>{wl.Name}</h1>
          <span>{wl.Namespace}</span>
        </div>
        <div className="detail-badges">
          {kindPill(isDB)}
          {risk != null ? riskPill(risk, reasons) : null}
          <Chip cls={isDB ? "db" : "k8s"}>{wl.DBProvider || (isDB ? "db" : "k8s")}</Chip>
        </div>
      </section>

      <div className="mt-6 grid grid-cols-1 gap-4 xl:grid-cols-2">
        <div className="card">
          <div className="card-head mb-2">
            <h2 className="card-title">Current state</h2>
          </div>
          <div className="kv">
            {curRows.map(([k, v]) => (
              <div className="row" key={String(k)}>
                <span className="k">{k}</span>
                <span className="v">{v}</span>
              </div>
            ))}
          </div>
        </div>

        <div className="card">
          <div className="card-head mb-2">
            <h2 className="card-title">Recommendation</h2>
          </div>
          {currentRecs.length === 0 ? (
            <div className="state">
              <p className="state-title">No current recommendation</p>
              <p className="state-sub">No change is waiting for this workload.</p>
            </div>
          ) : (
            <Table
              head={[
                { label: "Resource" },
                { label: "One-step plan" },
                { label: "Savings / mo", right: true },
                { label: "Confidence", right: true },
                { label: "Created" },
                { label: "Apply" },
              ]}
            >
              {currentRecs.map((r) => (
                <tr key={r.ID}>
                  <td>
                    <Chip>{r.Resource}</Chip>
                  </td>
                  <td>
                    {r.Resource === "class" ? (
                      <Delta from={r.ClassCurrent || "—"} to={r.ClassProposed || "—"} />
                    ) : (
                      <Delta from={fmtFrom(r)} to={fmtTo(r)} />
                    )}
                  </td>
                  <td className="num mono money">{money(r.SavingsMonthly)}</td>
                  <td className="num mono">{pct(r.Confidence)}</td>
                  <td className="text-faint">{fmtTime(r.CreatedAt)}</td>
                  <td>
                    {r.Status === "pending" && canApply ? (
                      <button className="btn small" onClick={() => setApplyRec(r)} type="button">
                        Apply
                      </button>
                    ) : (
                      <span className="text-faint">—</span>
                    )}
                  </td>
                </tr>
              ))}
            </Table>
          )}
        </div>

      </div>

      <div className="mt-4">
        <UsageChart workloadId={wl.ID} isDB={isDB} />
      </div>

      <div className="card mt-4">
        <div className="card-head">
          <h2 className="card-title">Apply history</h2>
          <span className="card-count">
            {applies.length} {applies.length === 1 ? "event" : "events"} for this workload
          </span>
        </div>
        <ApplyTimeline
          applies={applies}
          runsByEvent={runsByEvent}
          pendingRecommendations={currentRecs}
          canApply={canApply}
          onContinue={setApplyRec}
        />
      </div>

      {applyRec && (
        <ApplyModal
          rec={applyRec}
          onClose={() => setApplyRec(null)}
          onApplied={() => setReloadKey((k) => k + 1)}
        />
      )}
    </div>
  );
}

function fmtFrom(r: Recommendation): string {
  return r.Resource === "memory" ? fmtBytes(r.CurrentValue) : fmtMilli(r.CurrentValue);
}
function fmtTo(r: Recommendation): string {
  return r.Resource === "memory" ? fmtBytes(r.ProposedValue) : fmtMilli(r.ProposedValue);
}

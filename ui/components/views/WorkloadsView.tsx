"use client";

import React from "react";
import { api, pick } from "@/lib/api";
import type { Workload } from "@/lib/types";
import { fmtBytes, fmtMilli, isDBWorkload } from "@/lib/format";
import {
  Chip,
  EmptyState,
  ErrorState,
  LoadingState,
  PageHead,
  Table,
  WorkloadCell,
  kindPill,
  riskPill,
} from "@/components/ui";

type Filter = "all" | "compute" | "database";

export default function WorkloadsView() {
  const [workloads, setWorkloads] = React.useState<Workload[]>([]);
  const [filter, setFilter] = React.useState<Filter>("all");
  const [phase, setPhase] = React.useState<"loading" | "error" | "ready">("loading");

  React.useEffect(() => {
    let alive = true;
    api
      .workloads()
      .then((list) => {
        if (!alive) return;
        setWorkloads(list);
        setPhase("ready");
      })
      .catch(() => {
        if (alive) setPhase("error");
      });
    return () => {
      alive = false;
    };
  }, []);

  if (phase === "loading") return <LoadingState msg="Loading workloads…" />;
  if (phase === "error") return <ErrorState msg="Workloads failed to load." />;

  const rows = workloads.filter((w) => filter === "all" || (filter === "database") === isDBWorkload(w));
  const hasRisk = workloads.some((w) => pick<string>(w, ["Risk", "risk"]) != null);

  return (
    <div>
      <PageHead
        title="Workloads"
        sub="Resources currently under analysis."
      />

      <div className="card">
        <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-4">
          <div className="seg-row">
            {(["all", "compute", "database"] as const).map((f) => (
              <button
                key={f}
                className={`seg ${filter === f ? "active" : ""}`}
                onClick={() => setFilter(f)}
                type="button"
              >
                {f === "all" ? "All" : f.charAt(0).toUpperCase() + f.slice(1)}
              </button>
            ))}
          </div>
          <span className="card-count">{rows.length} workloads</span>
        </div>

        {workloads.length === 0 ? (
          <EmptyState msg="No workloads yet — run the collector." />
        ) : rows.length === 0 ? (
          <EmptyState msg="No workloads match this surface." />
        ) : (
          <div className="pt-3">
            <Table
              head={[
                { label: "Workload" },
                { label: "Kind" },
                { label: "Size" },
                { label: "Replicas" },
                { label: "Maintenance window" },
                { label: "Team" },
                { label: "Source" },
                ...(hasRisk ? [{ label: "Risk" }] : []),
              ]}
            >
              {rows.map((w) => {
                const db = isDBWorkload(w);
                return (
                  <tr key={w.ID}>
                    <td>
                      <WorkloadCell name={w.Name} namespace={w.Namespace} id={w.ID} isDB={db} />
                    </td>
                    <td>{kindPill(db)}</td>
                    <td>
                      {db ? (
                        <span className="delta">
                          <code>{w.DBClass || "—"}</code>
                          {w.DBProvider ? <Chip cls="prov">{w.DBProvider}</Chip> : null}
                        </span>
                      ) : (
                        <span className="mono text-muted">
                          {fmtMilli(w.RequestCPUMilli)} / {fmtBytes(w.RequestMemBytes)}
                        </span>
                      )}
                    </td>
                    <td className="mono text-faint">{db ? String(w.DBReplicas ?? "—") : "—"}</td>
                    <td className="mono text-faint">
                      {db && w.DBMaintenanceWindow ? w.DBMaintenanceWindow : "—"}
                    </td>
                    <td>
                      {w.TeamName ? (
                        <div className="flex flex-col gap-0.5">
                          <span className="text-[12px] font-medium text-ink">{w.TeamName}</span>
                          <span className="text-[11px] text-faint">{w.TeamOnCall || "on-call unassigned"}</span>
                        </div>
                      ) : (
                        <span className="text-faint">Unassigned</span>
                      )}
                    </td>
                    <td>
                      <Chip cls={db ? "db" : "k8s"}>{db ? "db" : "k8s"}</Chip>
                    </td>
                    {hasRisk &&
                      (pick<string>(w, ["Risk", "risk"]) != null ? (
                        <td>
                          {riskPill(
                            pick<string>(w, ["Risk", "risk"]),
                            pick<string[] | string>(w, ["RiskReasons", "risk_reasons"]),
                          )}
                        </td>
                      ) : (
                        <td className="text-faint">—</td>
                      ))}
                  </tr>
                );
              })}
            </Table>
          </div>
        )}
      </div>
    </div>
  );
}

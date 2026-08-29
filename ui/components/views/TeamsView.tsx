"use client";

import React from "react";
import { api } from "@/lib/api";
import type { Team, Workload } from "@/lib/types";
import { EmptyState, ErrorState, LoadingState, PageHead, Table, WorkloadCell } from "@/components/ui";

export default function TeamsView() {
  const [teams, setTeams] = React.useState<Team[]>([]);
  const [workloads, setWorkloads] = React.useState<Workload[]>([]);
  const [phase, setPhase] = React.useState<"loading" | "error" | "ready">("loading");

  React.useEffect(() => {
    let alive = true;
    Promise.all([api.teams(), api.workloads()])
      .then(([teamRows, workloadRows]) => {
        if (!alive) return;
        setTeams(teamRows);
        setWorkloads(workloadRows);
        setPhase("ready");
      })
      .catch(() => alive && setPhase("error"));
    return () => {
      alive = false;
    };
  }, []);

  if (phase === "loading") return <LoadingState msg="Loading ownership…" />;
  if (phase === "error") return <ErrorState msg="Ownership data failed to load." />;

  const assigned = new Map<number, Workload[]>();
  for (const workload of workloads) {
    if (workload.TeamID) assigned.set(workload.TeamID, [...(assigned.get(workload.TeamID) || []), workload]);
  }

  return (
    <div>
      <PageHead title="Ownership" sub="Deployment ownership metadata." />

      {teams.length === 0 ? (
        <div className="card"><EmptyState msg="No ownership metadata has been configured for this installation." /></div>
      ) : (
        <div className="ownership-grid">
          {teams.map((team) => {
            const rows = assigned.get(team.ID) || [];
            return (
              <section className="card ownership-card" key={team.ID}>
                <div className="ownership-card-head">
                  <div className="ownership-avatar">{team.Name.slice(0, 1).toUpperCase()}</div>
                  <div className="min-w-0">
                    <h2 className="ownership-name">{team.Name}</h2>
                    <p className="ownership-slug">{team.Slug}</p>
                  </div>
                  <span className="pill gray ml-auto">{rows.length} {rows.length === 1 ? "workload" : "workloads"}</span>
                </div>
                <dl className="kv ownership-kv">
                  <div className="row"><dt className="k">Owner</dt><dd className="v">{team.Owner || "Not configured"}</dd></div>
                  <div className="row"><dt className="k">On-call</dt><dd className="v">{team.OnCall || "Not configured"}</dd></div>
                </dl>
                {rows.length > 0 && (
                  <div className="ownership-workloads">
                    <div className="micro">Managed workloads</div>
                    <Table head={[{ label: "Workload" }, { label: "Namespace" }]}>
                      {rows.slice(0, 8).map((workload) => (
                        <tr key={workload.ID}>
                          <td><WorkloadCell name={workload.Name} namespace={workload.Namespace} id={workload.ID} isDB={workload.Source === "db"} /></td>
                          <td className="mono text-faint">{workload.Namespace}</td>
                        </tr>
                      ))}
                    </Table>
                    {rows.length > 8 && <p className="ownership-more">+ {rows.length - 8} more in Workloads</p>}
                  </div>
                )}
              </section>
            );
          })}
        </div>
      )}
    </div>
  );
}

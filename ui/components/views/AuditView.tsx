"use client";

import React from "react";
import { api } from "@/lib/api";
import type { ApplyEvent, VerificationRun, Workload } from "@/lib/types";
import { ErrorState, LoadingState, PageHead } from "@/components/ui";
import { ApplyTimeline } from "@/components/ApplyTimeline";

export default function AuditView() {
  const [phase, setPhase] = React.useState<"loading" | "error" | "ready">("loading");
  const [applies, setApplies] = React.useState<ApplyEvent[]>([]);
  const [runs, setRuns] = React.useState<VerificationRun[]>([]);
  const [nameOf, setNameOf] = React.useState<(id: number) => string>(() => () => "");

  React.useEffect(() => {
    let alive = true;
    Promise.all([api.applies(), api.verificationRuns(), api.workloads()])
      .then(([applyEvents, verificationRuns, workloads]) => {
        if (!alive) return;
        setApplies(applyEvents);
        setRuns(verificationRuns);
        const names = new Map<number, string>();
        (workloads as Workload[]).forEach((w) => names.set(w.ID, `${w.Namespace}/${w.Name}`));
        setNameOf(() => (id: number) => names.get(id) || `workload #${id}`);
        setPhase("ready");
      })
      .catch(() => {
        if (alive) setPhase("error");
      });
    return () => {
      alive = false;
    };
  }, []);

  if (phase === "loading") return <LoadingState msg="Loading audit trail…" />;
  if (phase === "error") return <ErrorState msg="Audit trail failed to load." />;

  const runsByEvent = new Map<number, VerificationRun>();
  runs.forEach((r) => runsByEvent.set(r.ApplyEventID, r));

  return (
    <div>
      <PageHead
        title="Audit"
        sub="Apply history and verification outcomes."
      />

      <div className="card">
        <div className="card-head">
          <h2 className="card-title">Apply &amp; verification timeline</h2>
          <span className="card-count">
            {applies.length} {applies.length === 1 ? "event" : "events"} · {runs.length}{" "}
            {runs.length === 1 ? "verification run" : "verification runs"}
          </span>
        </div>
        <ApplyTimeline applies={applies} runsByEvent={runsByEvent} nameOf={nameOf} />
      </div>
    </div>
  );
}

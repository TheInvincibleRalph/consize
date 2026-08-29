"use client";

import React from "react";
import type { ApplyEvent, VerificationRun } from "@/lib/types";
import { fmtBytes, fmtMilli, fmtTime, shortClass } from "@/lib/format";
import { SliSignals } from "@/components/SliSignals";
import { resultPill, verdictPill } from "@/components/ui";

export function ApplyTimeline({
  applies,
  runsByEvent,
  nameOf,
}: {
  applies: ApplyEvent[];
  runsByEvent: Map<number, VerificationRun>;
  nameOf?: (id: number) => string;
}) {
  if (!applies.length) {
    return (
      <div className="state">
        <p className="state-title">No apply events yet — the trail begins with the first apply.</p>
      </div>
    );
  }

  return (
    <ul className="timeline">
      {applies.map((e) => {
        const run = runsByEvent.get(e.ID);
        const dot = e.Result === "applied" ? "green" : e.Result === "reverted" ? "red" : "gray";
        const reverted = e.Result === "reverted";
        return (
          <li key={e.ID}>
            <span className={`tl-dot ${dot}`} />
            <div className={`tl-card ${reverted ? "reverted" : ""}`}>
              <div className="tl-top">
                <span className="mono text-faint">#{e.ID}</span>
                {resultPill(e.Result)}
                <span className="chip">{e.Mode}</span>
                <span className="mono text-faint">
                  step {e.StepNumber}/{e.TotalSteps}
                </span>
                {nameOf ? <span className="text-muted">{nameOf(e.WorkloadID)}</span> : null}
                <span className="tl-change">
                  <span className="delta">
                    <code>{fromOf(e)}</code>
                    <span className="arrow">→</span>
                    <code>{toOf(e)}</code>
                  </span>
                </span>
                <span className="tl-meta">
                  {e.Actor || "—"} · {fmtTime(e.CreatedAt)}
                </span>
              </div>

              {run ? (
                <div className="tl-verdict">
                  <div className="row">
                    {verdictPill(run.Verdict)}
                    <SliSignals slis={run.SLIs} compact />
                    <span className="tl-meta">
                      run #{run.ID} · {fmtTime(run.CreatedAt)}
                    </span>
                  </div>
                  <div className="row">
                    baseline {fmtTime(run.BaselineStart)} → {fmtTime(run.BaselineEnd)} · post{" "}
                    {fmtTime(run.PostStart)} → {fmtTime(run.PostEnd)}
                  </div>
                </div>
              ) : e.Result === "applied" ? (
                <div className="tl-verdict">
                  <span className="tl-empty">no verification run yet</span>
                </div>
              ) : null}
            </div>
          </li>
        );
      })}
    </ul>
  );
}

/* Diff cell: class events name the change via current_class/proposed_class
 * (request/limit are 0), request/limit events via the request values. */
function fromOf(e: ApplyEvent): string {
  const d = e.Diff;
  if (!d?.resource) return "—";
  if (d.resource === "class") return shortClass(d.current_class);
  if (d.resource === "memory") return fmtBytes(d.current_request);
  if (d.resource === "cpu") return fmtMilli(d.current_request);
  return "—";
}

function toOf(e: ApplyEvent): string {
  const d = e.Diff;
  if (!d?.resource) return "—";
  if (d.resource === "class") return shortClass(d.proposed_class);
  if (d.resource === "memory") return fmtBytes(d.proposed_request);
  if (d.resource === "cpu") return fmtMilli(d.proposed_request);
  return "—";
}

"use client";

import React from "react";
import type { ApplyEvent, Recommendation, VerificationRun } from "@/lib/types";
import { fmtBytes, fmtMilli, fmtTime, shortClass } from "@/lib/format";
import { SliSignals } from "@/components/SliSignals";
import { Delta, resultPill, verdictPill } from "@/components/ui";

export function ApplyTimeline({
  applies,
  runsByEvent,
  nameOf,
  pendingRecommendations = [],
  canApply = false,
  onContinue,
}: {
  applies: ApplyEvent[];
  runsByEvent: Map<number, VerificationRun>;
  nameOf?: (id: number) => string;
  pendingRecommendations?: Recommendation[];
  canApply?: boolean;
  onContinue?: (rec: Recommendation) => void;
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
        const workloadName = nameOf ? nameOf(e.WorkloadID) : "";
        const hasMoreSteps = e.Result === "applied" && e.StepNumber > 0 && e.TotalSteps > e.StepNumber;
        const continuation = hasMoreSteps ? continuationFor(e, pendingRecommendations) : null;
        return (
          <li key={e.ID}>
            <span className={`tl-dot ${dot}`} />
            <div className={`tl-card ${reverted ? "reverted" : ""}`}>
              <div className="tl-event-head">
                <div className="tl-event-main">
                  <span className="tl-event-id">#{e.ID}</span>
                  <div className="tl-event-copy">
                    <div className="tl-mainline">
                      <span>{resourceLabel(e)} changed</span>
                      <Delta from={fromOf(e)} to={toOf(e)} />
                    </div>
                    <div className="tl-subrow">
                      {resultPill(e.Result)}
                      <span>{e.Mode}</span>
                      <span>Step {e.StepNumber}/{e.TotalSteps}</span>
                      {workloadName ? <span>{workloadName}</span> : null}
                    </div>
                  </div>
                </div>
                <div className="tl-event-meta">
                  <span>{e.Actor || "—"}</span>
                  <time dateTime={e.CreatedAt}>{shortDateTime(e.CreatedAt)}</time>
                </div>
              </div>

              {run ? (
                <div className="tl-check-panel">
                  <div className="tl-check-head">
                    <div className="tl-check-title">
                      {verdictPill(run.Verdict)}
                      <span>{verificationCopy(run.Verdict)}</span>
                    </div>
                    <div className="tl-check-actions">
                      {run.Verdict === "passed" && continuation && onContinue ? (
                        <button className="btn small" type="button" onClick={() => onContinue(continuation)} disabled={!canApply}>
                          Continue step {e.StepNumber + 1}/{e.TotalSteps}
                        </button>
                      ) : null}
                      <span className="tl-run-meta">Run #{run.ID} · {shortDateTime(run.CreatedAt)}</span>
                    </div>
                  </div>
                  <div className="tl-sli-row">
                    <SliSignals slis={run.SLIs} compact />
                  </div>
                  {run.Verdict === "passed" && hasMoreSteps && !continuation ? (
                    <p className="tl-next-note">Next step is waiting for fresh analysis or has already been completed.</p>
                  ) : null}
                  <details className="tl-window-disclosure">
                    <summary>Verification window</summary>
                    <div className="tl-window-row">
                      <span>Baseline {compactRange(run.BaselineStart, run.BaselineEnd)}</span>
                      <span>Post-change {compactRange(run.PostStart, run.PostEnd)}</span>
                    </div>
                  </details>
                </div>
              ) : e.Result === "applied" ? (
                <div className="tl-check-panel pending">
                  <span className="tl-empty">Safety check scheduled</span>
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

function resourceLabel(e: ApplyEvent): string {
  const resource = e.Diff?.resource;
  if (resource === "class") return "Database class";
  if (resource === "memory") return "Memory";
  if (resource === "cpu") return "CPU";
  return "Resource";
}

function verificationCopy(verdict?: string): string {
  switch (verdict) {
    case "passed":
      return "Safety checks passed";
    case "failed":
      return "Safety checks failed";
    case "inconclusive":
      return "Safety checks inconclusive";
    default:
      return "Safety checks recorded";
  }
}

function shortDateTime(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

function shortTime(iso?: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  return d.toLocaleTimeString(undefined, {
    hour: "numeric",
    minute: "2-digit",
  });
}

function compactRange(start?: string | null, end?: string | null): string {
  if (!start || !end) return "—";
  const a = new Date(start);
  const b = new Date(end);
  if (Number.isNaN(a.getTime()) || Number.isNaN(b.getTime())) {
    return `${fmtTime(start)} → ${fmtTime(end)}`;
  }
  const sameDay = a.toDateString() === b.toDateString();
  if (sameDay) return `${shortTime(start)}–${shortTime(end)}`;
  return `${shortDateTime(start)}–${shortDateTime(end)}`;
}

export function continuationFor(e: ApplyEvent, recs: Recommendation[]): Recommendation | null {
  const resource = e.Diff?.resource;
  if (!resource) return null;
  return (
    recs.find((r) => {
      if (r.Status !== "pending") return false;
      if (r.WorkloadID !== e.WorkloadID) return false;
      if (r.Resource !== resource) return false;
      if (resource === "class") return r.ClassCurrent === e.Diff?.proposed_class;
      return Number(r.CurrentValue) === Number(e.Diff?.proposed_request);
    }) || null
  );
}

"use client";

import React from "react";
import { api, pick } from "@/lib/api";
import type { Recommendation } from "@/lib/types";
import { currentOf, fmtTime, money, pct, proposedOf } from "@/lib/format";
import {
  EmptyState,
  ErrorState,
  LoadingState,
  PageHead,
  Table,
  WorkloadCell,
  kindPill,
  riskPill,
} from "@/components/ui";
import { ApplyModal } from "@/components/ApplyModal";
import { useAuth } from "@/components/auth";

const PAGE = 100;

interface ViewState {
  all: Recommendation[];
  shown: number;
  total: number | null;
}

const INITIAL: ViewState = { all: [], shown: 0, total: null };

export default function RecommendationsView() {
  const { authEnabled, user } = useAuth();
  
  
  const canApply = !authEnabled || user?.role === "operator" || user?.role === "admin";
  const [data, setData] = React.useState<ViewState>(INITIAL);
  const [phase, setPhase] = React.useState<"loading" | "error" | "ready">("loading");
  const [busy, setBusy] = React.useState(false);
  const [applyRec, setApplyRec] = React.useState<Recommendation | null>(null);
  const [reloadKey, setReloadKey] = React.useState(0);

  // ref mirror of the current row count so overlapping "load more"
  
  const lenRef = React.useRef(0);
  const busyRef = React.useRef(false);

  const loadMore = React.useCallback(async (reset = false) => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    try {
      const offset = reset ? 0 : lenRef.current;
      const body = await api.recommendations({ status: "pending", offset, limit: PAGE });
      const fresh = body.recommendations || [];
      const page = body.pagination;
      setData((prev) => {
        const base = reset ? [] : prev.all;
        const seen = new Set(base.map((r) => r.ID));
        const added = fresh.filter((r) => !seen.has(r.ID));
        const next = [...base, ...added].sort(
          (a, b) => (b.SavingsMonthly || 0) - (a.SavingsMonthly || 0),
        );
        lenRef.current = next.length;
        if (page && typeof page.total === "number") {
          return { all: next, shown: next.length, total: page.total };
        }
        return { all: next, shown: Math.min(next.length, prev.shown + PAGE), total: prev.total };
      });
      setPhase("ready");
    } catch {
      setPhase("error");
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }, []);

  // (Re)load on mount and after an apply closes (statuses may have changed).
  React.useEffect(() => {
    lenRef.current = 0;
    void loadMore(true);
  }, [reloadKey, loadMore]);

  if (phase === "loading") return <LoadingState msg="Loading recommendations…" />;
  if (phase === "error") return <ErrorState msg="Recommendations failed to load." />;

  const { all, shown, total } = data;
  const visible = all.slice(0, shown);
  const noMore = total != null ? shown >= total : shown >= all.length;
  const hasRisk = all.some((r) => pick<string>(r, ["Risk", "risk"]) != null);

  const head = [
    { label: "Workload" },
    { label: "Kind" },
    { label: "One-step plan" },
    { label: "Savings / mo", right: true },
    { label: "Confidence", right: true },
    ...(hasRisk ? [{ label: "Risk" }] : []),
    { label: "Created" },
    { label: "Apply" },
  ];

  return (
    <div>
      <PageHead
        title="Recommendations"
        sub="Open optimization decisions ranked by monthly impact."
      />

      <div className="card">
        {all.length === 0 ? (
          <EmptyState msg="No recommendations right now." />
        ) : (
          <>
            <Table head={head}>
              {visible.map((r) => {
                const db = r.Resource === "class";
                const risk = pick<string>(r, ["Risk", "risk"]);
                const reasons = pick<string[] | string>(r, ["RiskReasons", "risk_reasons"]);
                return (
                  <tr key={r.ID}>
                    <td>
                      <WorkloadCell
                        name={r.WorkloadName}
                        namespace={r.Namespace}
                        id={r.WorkloadID}
                        isDB={db}
                      />
                    </td>
                    <td>{kindPill(db)}</td>
                    <td>
                      {db ? (
                        <span className="delta">
                          <span className="muted-text">class </span>
                          <code>{r.ClassCurrent || "—"}</code>
                          <span className="arrow">→</span>
                          <code>{r.ClassProposed || "—"}</code>
                        </span>
                      ) : (
                        <span className="delta">
                          <code>{currentOf(r)}</code>
                          <span className="arrow">→</span>
                          <code>{proposedOf(r)}</code>
                        </span>
                      )}
                    </td>
                    <td className="num mono money">{money(r.SavingsMonthly)}</td>
                    <td className="num mono">{pct(r.Confidence)}</td>
                    {hasRisk &&
                      (risk != null ? (
                        <td>{riskPill(risk, reasons)}</td>
                      ) : (
                        <td className="text-faint">—</td>
                      ))}
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
                );
              })}
            </Table>
            <div className="load-row">
              <button
                className="btn"
                disabled={noMore || !all.length}
                onClick={() => loadMore()}
                type="button"
              >
                {busy ? "Loading…" : "Load more"}
              </button>
              <span className="count">
                Showing {visible.length} of {total != null ? total : all.length} recommendations ·
                current only
              </span>
            </div>
          </>
        )}
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

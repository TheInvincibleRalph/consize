"use client";

import React from "react";
import { CloudOff, ExternalLink, GitPullRequest, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import type { CostOpportunity, IaCPullRequest } from "@/lib/types";
import { fmtTime, money } from "@/lib/format";
import { useAuth } from "@/components/auth";
import { Chip, EmptyState, ErrorState, LoadingState, PageHead, Pill } from "@/components/ui";

type Phase = "loading" | "ready" | "error";

function typeLabel(type: string): string {
  switch (type) {
    case "unattached_volume":
      return "Unattached volume";
    case "idle_load_balancer":
      return "Idle load balancer";
    case "unused_nat_gateway":
      return "Unused NAT gateway";
    case "stopped_instance":
      return "Stopped instance";
    default:
      return type.replaceAll("_", " ");
  }
}

function errMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const body = err.body as { error?: string } | null;
    return body?.error ? `${err.message}: ${body.error}` : err.message;
  }
  return String((err as Error)?.message || err);
}

function evidenceItems(evidence?: Record<string, unknown>): [string, unknown][] {
  if (!evidence) return [];
  return Object.entries(evidence).sort(([a], [b]) => a.localeCompare(b));
}

export default function CostOpportunitiesView() {
  const { authEnabled, user } = useAuth();
  const canWrite = !authEnabled || user?.role === "operator" || user?.role === "admin";
  const [phase, setPhase] = React.useState<Phase>("loading");
  const [opportunities, setOpportunities] = React.useState<CostOpportunity[]>([]);
  const [prs, setPRs] = React.useState<Record<string, IaCPullRequest>>({});
  const [busy, setBusy] = React.useState<"scan" | number | null>(null);
  const [message, setMessage] = React.useState("");
  const [selectedPR, setSelectedPR] = React.useState<IaCPullRequest | null>(null);

  const load = React.useCallback(() => {
    api
      .costOpportunities()
      .then((body) => {
        setOpportunities(body.opportunities || []);
        setPRs(body.latest_prs || {});
        setPhase("ready");
      })
      .catch(() => setPhase("error"));
  }, []);

  React.useEffect(() => {
    load();
  }, [load]);

  const scan = async () => {
    setBusy("scan");
    setMessage("");
    try {
      const body = await api.scanCostOpportunities();
      setOpportunities(body.opportunities || []);
      setMessage(`Scan complete: ${body.opportunities?.length || 0} open opportunities.`);
    } catch (err) {
      setMessage(errMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const openPR = async (opp: CostOpportunity) => {
    setBusy(opp.ID);
    setMessage("");
    try {
      const body = await api.prepareIaCPullRequest(opp.ID);
      setSelectedPR(body.pull_request);
      setPRs((current) => ({ ...current, [String(opp.ID)]: body.pull_request }));
      setOpportunities((current) =>
        current.map((item) =>
          item.ID === opp.ID ? { ...item, Status: "pr_ready" } : item,
        ),
      );
    } catch (err) {
      setMessage(errMessage(err));
    } finally {
      setBusy(null);
    }
  };

  if (phase === "loading") return <LoadingState msg="Loading cloud waste…" />;
  if (phase === "error") return <ErrorState msg="Cloud waste failed to load." />;

  const open = opportunities.filter((opp) => opp.Status === "open" || opp.Status === "pr_ready");
  const total = open.reduce((sum, opp) => sum + (opp.MonthlyCost || 0), 0);
  const top = [...open].sort((a, b) => (b.MonthlyCost || 0) - (a.MonthlyCost || 0))[0];

  return (
    <div>
      <PageHead title="Cloud waste" sub="Unattached and idle cloud resources still accruing cost." />

      <div className="cost-summary">
        <section className="kpi-card">
          <div className="kpi-icon" aria-hidden="true">
            <CloudOff size={18} />
          </div>
          <div>
            <div className="kpi-label">Open opportunities</div>
            <div className="kpi-value">{open.length}</div>
          </div>
        </section>
        <section className="kpi-card money">
          <div className="kpi-icon" aria-hidden="true">
            <CloudOff size={18} />
          </div>
          <div>
            <div className="kpi-label">Estimated savings</div>
            <div className="kpi-value">{money(total)}</div>
          </div>
        </section>
        <section className="card cost-action-card">
          <div>
            <h2 className="card-title">Scan cloud inventory</h2>
            <p className="card-sub">Find unattached storage, idle load balancers, unused NAT gateways, and stopped instances.</p>
          </div>
          <button className="btn primary" type="button" onClick={scan} disabled={!canWrite || busy === "scan"}>
            <RefreshCw size={14} className={busy === "scan" ? "spin" : ""} />
            {busy === "scan" ? "Scanning…" : "Run scan"}
          </button>
        </section>
      </div>

      {message && <p className="alerting-message mt-4">{message}</p>}

      {opportunities.length === 0 ? (
        <div className="card mt-4">
          <EmptyState msg="No cloud waste opportunities yet." />
        </div>
      ) : (
        <div className="cost-grid mt-4">
          <section className="card cost-list-card">
            <div className="card-head">
              <h2 className="card-title">Opportunities</h2>
              <span className="card-count">{open.length} open</span>
            </div>
            <div className="cost-list">
              {opportunities.map((opp) => {
                const pr = prs[String(opp.ID)];
                return (
                  <article className="cost-row" key={opp.ID}>
                    <div className="cost-row-main">
                      <div className="cost-row-title">
                        <span>{opp.Name || opp.ResourceID}</span>
                        <Pill color="gray">{typeLabel(opp.ResourceType)}</Pill>
                        <Pill color={opp.Risk === "high" ? "red" : opp.Risk === "medium" ? "amber" : "green"}>{opp.Risk || "low"}</Pill>
                      </div>
                      <div className="cost-row-meta">
                        <span>{opp.Provider}</span>
                        <span>{opp.Region}</span>
                        <span>{opp.ResourceID}</span>
                        <span>seen {fmtTime(opp.LastSeenAt)}</span>
                      </div>
                      <div className="cost-evidence">
                        {evidenceItems(opp.Evidence).slice(0, 4).map(([key, value]) => (
                          <Chip key={key}>{key}: {String(value)}</Chip>
                        ))}
                      </div>
                    </div>
                    <div className="cost-row-side">
                      <strong>{money(opp.MonthlyCost)}</strong>
                      <span>/ mo</span>
                      {pr ? (
                        <button className="btn small" type="button" onClick={() => setSelectedPR(pr)}>
                          View PR
                        </button>
                      ) : (
                        <button className="btn small" type="button" onClick={() => openPR(opp)} disabled={!canWrite || busy === opp.ID}>
                          <GitPullRequest size={13} />
                          {busy === opp.ID ? "Opening…" : "Open PR"}
                        </button>
                      )}
                    </div>
                  </article>
                );
              })}
            </div>
          </section>

          <aside className="card cost-pr-card">
            <div className="card-head">
              <h2 className="card-title">Terraform workflow</h2>
            </div>
            {selectedPR ? (
              <div className="cost-pr-preview">
                <Pill color="gray">{selectedPR.Status}</Pill>
                <h3>{selectedPR.Title}</h3>
                <dl className="kv">
                  <div className="row"><dt className="k">Repo</dt><dd className="v">{selectedPR.Repo || "Not configured"}</dd></div>
                  <div className="row"><dt className="k">Branch</dt><dd className="v mono">{selectedPR.Branch}</dd></div>
                  {selectedPR.URL && (
                    <div className="row">
                      <dt className="k">Pull request</dt>
                      <dd className="v">
                        <a className="link-row" href={selectedPR.URL} target="_blank" rel="noreferrer">
                          Open in GitHub <ExternalLink size={12} />
                        </a>
                      </dd>
                    </div>
                  )}
                </dl>
                {selectedPR.Error && <div className="pr-error-box">{selectedPR.Error}</div>}
                <pre>{selectedPR.Diff}</pre>
              </div>
            ) : (
              <div className="cost-pr-empty">
                <GitPullRequest size={18} />
                <p>Select an opportunity to open a Terraform PR.</p>
                {top && <span>Highest impact: {top.Name || top.ResourceID} · {money(top.MonthlyCost)}/mo</span>}
              </div>
            )}
          </aside>
        </div>
      )}
    </div>
  );
}

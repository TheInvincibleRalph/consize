"use client";

import React from "react";
import { Download, Send } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { fmtTime, money } from "@/lib/format";
import type { AlertDeliveryResult, ReportConfig, SavingsReport } from "@/lib/types";
import { useAuth } from "@/components/auth";
import { EmptyState, ErrorState, LoadingState, PageHead, Pill } from "@/components/ui";

const REPORT_RANGES = [7, 14, 30] as const;

function errMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const body = err.body as { error?: string } | null;
    return body?.error ? `${err.message}: ${body.error}` : err.message;
  }
  return String((err as Error)?.message || err);
}

function deliveryText(results: AlertDeliveryResult[] | null): string {
  if (!results) return "";
  if (results.length === 0) return "No Slack route matched this report.";
  const failed = results.filter((result) => result.status === "failed");
  if (failed.length > 0) return `${failed.length} delivery failed.`;
  return "Report sent to Slack.";
}

export default function ReportsView() {
  const { authEnabled, user } = useAuth();
  const canWrite = !authEnabled || user?.role === "admin";
  const [phase, setPhase] = React.useState<"loading" | "ready" | "error">("loading");
  const [config, setConfig] = React.useState<ReportConfig>({ enabled: false, range_days: 7 });
  const [rangeDays, setRangeDays] = React.useState<number>(7);
  const [report, setReport] = React.useState<SavingsReport | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [sending, setSending] = React.useState(false);
  const [message, setMessage] = React.useState("");
  const [results, setResults] = React.useState<AlertDeliveryResult[] | null>(null);

  const loadReport = React.useCallback((days: number) => {
    api
      .savingsReport(days)
      .then((body) => setReport(body.report))
      .catch(() => setPhase("error"));
  }, []);

  React.useEffect(() => {
    let alive = true;
    Promise.all([api.reportConfig(), api.savingsReport(7)])
      .then(([cfgBody, reportBody]) => {
        if (!alive) return;
        const next = cfgBody.config || { enabled: false, range_days: 7 };
        setConfig(next);
        setRangeDays(next.range_days || 7);
        setReport(reportBody.report);
        if ((next.range_days || 7) !== 7) loadReport(next.range_days);
        setPhase("ready");
      })
      .catch(() => alive && setPhase("error"));
    return () => {
      alive = false;
    };
  }, [loadReport]);

  const updateRange = (days: number) => {
    setRangeDays(days);
    setMessage("");
    setResults(null);
    loadReport(days);
  };

  const save = async () => {
    setSaving(true);
    setMessage("");
    setResults(null);
    try {
      const body = await api.saveReportConfig(config);
      setConfig(body.config);
      setMessage("Report settings saved.");
    } catch (err) {
      setMessage(errMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const send = async () => {
    setSending(true);
    setMessage("");
    setResults(null);
    try {
      const body = await api.sendReport(rangeDays);
      setReport(body.report);
      setResults(body.results || []);
      setMessage(deliveryText(body.results || []));
    } catch (err) {
      const apiErr = err instanceof ApiError ? (err.body as { results?: AlertDeliveryResult[] } | null) : null;
      if (apiErr?.results) setResults(apiErr.results);
      setMessage(errMessage(err));
    } finally {
      setSending(false);
    }
  };

  if (phase === "loading") return <LoadingState msg="Loading reports…" />;
  if (phase === "error") return <ErrorState msg="Reports failed to load." />;

  return (
    <div>
      <PageHead title="Reports" sub="Savings summaries, exports, and scheduled delivery." />

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[1.25fr_0.75fr]">
        <div className="space-y-4">
          <section className="card report-summary">
            <div className="card-head">
              <div>
                <h2 className="card-title">{rangeDays}-day savings report</h2>
                <p className="card-sub">
                  {report ? `${fmtTime(report.from)} — ${fmtTime(report.to)}` : "No report generated yet."}
                </p>
              </div>
              <div className="report-range">
                {REPORT_RANGES.map((days) => (
                  <button
                    key={days}
                    type="button"
                    className={days === rangeDays ? "active" : ""}
                    onClick={() => updateRange(days)}
                  >
                    {days === 7 ? "Week" : `${days} days`}
                  </button>
                ))}
              </div>
            </div>

            {report && (
              <div className="report-metrics">
                <div>
                  <span>Realized this period</span>
                  <strong>{money(report.realized_this_period_monthly_savings)}</strong>
                  <small>monthly savings from verified applies</small>
                </div>
                <div>
                  <span>Pending opportunity</span>
                  <strong>{money(report.projected_monthly_savings)}</strong>
                  <small>{report.pending_recommendations} recommendations</small>
                </div>
                <div>
                  <span>Verified applies</span>
                  <strong>{report.verified_applies}</strong>
                  <small>{report.rollbacks} rollbacks</small>
                </div>
                <div>
                  <span>Verification issues</span>
                  <strong>{report.failed_verifications + report.inconclusive_verifications}</strong>
                  <small>{report.failed_verifications} failed · {report.inconclusive_verifications} inconclusive</small>
                </div>
              </div>
            )}
          </section>

          <section className="card report-section">
            <div className="card-head">
              <div>
                <h2 className="card-title">Top pending recommendations</h2>
              </div>
            </div>
            {!report || report.top_pending_recommendations.length === 0 ? (
              <EmptyState msg="No pending recommendations." />
            ) : (
              <div className="tbl-wrap">
                <table className="data">
                  <thead>
                    <tr>
                      <th>Workload</th>
                      <th>Recommendation</th>
                      <th className="num">Savings / mo</th>
                    </tr>
                  </thead>
                  <tbody>
                    {report.top_pending_recommendations.map((rec) => (
                      <tr key={rec.id}>
                        <td>
                          <div className="wl">
                            <strong>{rec.workload_name}</strong>
                            <span>{rec.namespace}</span>
                          </div>
                        </td>
                        <td>
                          <span className="delta">
                            <code>{rec.current}</code>
                            <span className="arrow">→</span>
                            <code>{rec.proposed}</code>
                          </span>
                        </td>
                        <td className="num money">{money(rec.savings_monthly)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section className="card report-section">
            <div className="card-head">
              <div>
                <h2 className="card-title">Data quality</h2>
              </div>
              {report?.latest_usage_bucket && <Pill color="gray">latest bucket {fmtTime(report.latest_usage_bucket)}</Pill>}
            </div>
            <div className="report-quality">
              {(report?.data_quality || []).map((item) => (
                <p key={item}>{item}</p>
              ))}
            </div>
          </section>
        </div>

        <aside className="space-y-4">
          <section className="card report-section">
            <div className="card-head">
              <div>
                <h2 className="card-title">Weekly digest</h2>
              </div>
              <Pill color={config.enabled ? "green" : "gray"}>{config.enabled ? "On" : "Off"}</Pill>
            </div>
            <div className="report-controls">
              <label className="config-check compact">
                <input
                  type="checkbox"
                  checked={config.enabled}
                  disabled={!canWrite}
                  onChange={(e) => setConfig((current) => ({ ...current, enabled: e.target.checked }))}
                />
                Send weekly Slack digest
              </label>
              <label className="config-field">
                <span>Default report range</span>
                <select
                  className="config-input"
                  value={config.range_days}
                  disabled={!canWrite}
                  onChange={(e) => setConfig((current) => ({ ...current, range_days: Number(e.target.value) }))}
                >
                  {REPORT_RANGES.map((days) => (
                    <option key={days} value={days}>{days} days</option>
                  ))}
                </select>
              </label>
              <button className="btn primary" type="button" onClick={save} disabled={!canWrite || saving}>
                {saving ? "Saving…" : "Save settings"}
              </button>
            </div>
          </section>

          <section className="card report-section">
            <div className="card-head">
              <div>
                <h2 className="card-title">Generate</h2>
              </div>
            </div>
            <div className="report-controls">
              <button className="btn" type="button" onClick={send} disabled={!canWrite || sending}>
                <Send size={14} /> {sending ? "Sending…" : "Send to Slack"}
              </button>
              <a className="btn" href={api.reportPdfUrl(rangeDays)} download>
                <Download size={14} /> Download PDF
              </a>
              {message && <p className="alerting-message">{message}</p>}
              {results && results.length > 0 && (
                <div className="alerting-results">
                  {results.map((result, index) => (
                    <div className={`alerting-result ${result.status === "sent" ? "ok" : "bad"}`} key={`${result.contact_point}-${index}`}>
                      <span className="mono">{result.contact_point}</span>
                      <span>{result.type}</span>
                      <span>{result.status}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </section>
        </aside>
      </div>
    </div>
  );
}

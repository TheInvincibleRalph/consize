"use client";

import React from "react";
import { ExternalLink, GitPullRequest } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import type { ApplyDiff, ApplyResult, IaCPullRequest, Recommendation } from "@/lib/types";
import { currentOf, diffLine, proposedOf, shortClass } from "@/lib/format";
import { useAuth } from "@/components/auth";

export type ApplyMode = "dry_run" | "approved" | "auto";
type Delivery = "direct" | "iac";

const MODES: { key: ApplyMode; label: string }[] = [
  { key: "dry_run", label: "Dry run" },
  { key: "approved", label: "approved" },
  { key: "auto", label: "auto" },
];

export function ApplyModal({
  rec,
  onClose,
  onApplied,
}: {
  rec: Recommendation;
  onClose: () => void;
  onApplied: () => void;
}) {
  const { authEnabled } = useAuth();
  const [delivery, setDelivery] = React.useState<Delivery>("iac");
  const [mode, setMode] = React.useState<ApplyMode>("dry_run");
  const [actor, setActor] = React.useState("");
  const [repo, setRepo] = React.useState("");
  const [path, setPath] = React.useState("");
  const [resourceTarget, setResourceTarget] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [copied, setCopied] = React.useState(false);
  const [outcome, setOutcome] = React.useState<React.ReactNode>(null);

  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  function run() {
    if (busy) return;
    if (delivery === "iac") {
      preparePRPlan();
      return;
    }
    const payload: { mode: ApplyMode; actor?: string } = { mode };
    if (mode === "approved" && !authEnabled) {
      // Without auth the server still wants an actor string for approved
      
      const a = actor.trim();
      if (!a) {
        setOutcome(<p className="err">Approved mode requires an actor.</p>);
        return;
      }
      payload.actor = a;
    }
    setBusy(true);
    setOutcome(<p className="muted-text">Applying…</p>);
    api
      .apply(rec.ID, payload)
      .then((body) => {
        setBusy(false);
        setOutcome(applyOutcome(body));
        onApplied();
      })
      .catch((err: unknown) => {
        setBusy(false);
        setOutcome(applyError(err));
      });
  }

  function preparePRPlan() {
    const repoValue = repo.trim();
    const pathValue = path.trim();
    const terraformTarget = resourceTarget.trim();
    const payload: { repo?: string; path?: string; terraform_addr?: string; actor?: string } = {
      repo: repoValue || (looksLikeURL(pathValue) ? pathValue : undefined),
      path: pathValue && !looksLikeURL(pathValue) ? pathValue : undefined,
      terraform_addr: shouldSendTerraformTarget(pathValue) && terraformTarget ? terraformTarget : undefined,
    };
    if (!authEnabled) {
      const a = actor.trim();
      if (a) payload.actor = a;
    }
    setBusy(true);
    setCopied(false);
    setOutcome(<p className="muted-text">Opening IaC pull request…</p>);
    api
      .prepareRecommendationIaCPullRequest(rec.ID, payload)
      .then((body) => {
        setBusy(false);
        setOutcome(prOutcome(body.pull_request));
        onApplied();
      })
      .catch((err: unknown) => {
        setBusy(false);
        setOutcome(applyError(err));
      });
  }

  function applyOutcome(b: ApplyResult) {
    const parts: React.ReactNode[] = [];
    if (b.Blocked) {
      parts.push(blockedBox("Apply blocked", b.BlockReasons, "blocked"));
    } else {
      // Step plan: "step 1 of 4 — medium" (short class family for class
      // recommendations).
      if (b.StepNumber || b.TotalSteps) {
        let text = `Step ${b.StepNumber} of ${b.TotalSteps}`;
        if (b.Diff?.resource === "class" && b.Diff.proposed_class) {
          text += ` — ${shortClass(b.Diff.proposed_class)}`;
        }
        parts.push(<p className="plan" key="step-plan">{text}</p>);
      }
      if (b.Diff?.resource) parts.push(<p className="change" key="diff">{diffLine(b.Diff as ApplyDiff)}</p>);
      // Maintenance-window state (DB engine only).
      if (b.InWindow !== undefined) {
        parts.push(
          <p className={`window ${b.InWindow ? "in" : "out"}`} key="maintenance-window">
            {b.InWindow ? "In maintenance window" : "Outside maintenance window"}
            {b.Window ? ` (${b.Window})` : ""}
          </p>,
        );
      }
      if (b.DryRun) parts.push(<p className="ok" key="dry-run">Dry run — nothing was changed.</p>);
      if (b.Applied) {
        parts.push(
          <p className="ok" key="applied">
            Applied. Consize will verify this change automatically before the next step.
          </p>,
        );
      }
      if (!parts.length) parts.push(<p className="muted-text" key="done">Done — no details returned.</p>);
    }
    return <div className="flex flex-col gap-1.5">{parts}</div>;
  }

  function applyError(e: unknown) {
    if (e instanceof ApiError && e.status === 422) {
      const body = e.body as { error?: string; reasons?: unknown };
      return blockedBox("Apply blocked", body?.reasons);
    }
    const body = (e instanceof ApiError && (e.body as { error?: string })) || null;
    return <p className="err">{formatErrorMessage(body?.error || String((e as Error)?.message || e))}</p>;
  }

  function blockedBox(title: string, reasons: unknown, key?: React.Key) {
    const list = Array.isArray(reasons) ? reasons.map((r) => formatGuardrailReason(String(r))) : [];
    const normalizedTitle = list.length === 1 && list[0].title ? list[0].title : title;
    return (
      <div className="blocked" key={key}>
        <p className="blocked-title">{normalizedTitle}</p>
        {list.length ? (
          <div className="reasons">
            {list.map((reason, i) => (
              <p key={i}>{reason.message}</p>
            ))}
          </div>
        ) : (
          <p className="muted-text">No reasons given.</p>
        )}
      </div>
    );
  }

  function prOutcome(pr: IaCPullRequest) {
    const opened = pr.Status === "opened" && pr.URL;
    return (
      <div className="pr-plan-result">
        <div className="pr-plan-actions">
          <p className={opened ? "ok" : pr.Status === "failed" ? "err" : "ok"}>
            {opened ? "IaC PR opened." : pr.Status === "failed" ? "IaC PR failed." : "IaC PR plan prepared."}
          </p>
          <button
            className="btn small"
            type="button"
            onClick={() => {
              void navigator.clipboard?.writeText(pr.Diff || "").then(() => setCopied(true));
            }}
          >
            {copied ? "Copied" : "Copy diff"}
          </button>
        </div>
        <div className="kv compact">
          <div className="row">
            <span className="k">Status</span>
            <span className="v">{pr.Status}</span>
          </div>
          <div className="row">
            <span className="k">Branch</span>
            <span className="v mono">{pr.Branch}</span>
          </div>
          <div className="row">
            <span className="k">Repo</span>
            <span className="v">{pr.Repo || "Not configured"}</span>
          </div>
          {pr.URL && (
            <div className="row">
              <span className="k">Pull request</span>
              <a className="v link-row" href={pr.URL} target="_blank" rel="noreferrer">
                Open in GitHub <ExternalLink size={12} />
              </a>
            </div>
          )}
        </div>
        {pr.Error && <div className="pr-error-box">{pr.Error}</div>}
        <pre>{pr.Diff}</pre>
      </div>
    );
  }

  const isDB = rec.Resource === "class";
  const defaultResourceTarget = defaultRecommendationResourceTarget(rec);
  const sourceKind = sourcePathKind(path);
  const showResourceTarget = sourceKind !== "yaml" && sourceKind !== "helm-values";

  return (
    <div
      className="modal-overlay"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="modal" role="dialog" aria-modal="true" aria-label="Apply recommendation">
        <div className="modal-head">
          <h2>Apply recommendation</h2>
          <p>
            {rec.WorkloadName} · {rec.Namespace} · {rec.Resource}
          </p>
        </div>

        <div className="modal-change">
          <span className="delta">
            <code>{isDB ? rec.ClassCurrent : currentOf(rec)}</code>
            <span className="arrow">→</span>
            <code>{isDB ? rec.ClassProposed : proposedOf(rec)}</code>
          </span>
        </div>

        <div className="delivery-choice">
          <button
            className={`delivery-card ${delivery === "direct" ? "active" : ""}`}
            type="button"
            onClick={() => {
              setDelivery("direct");
              setOutcome(null);
            }}
          >
            <span>Direct apply</span>
            <small>Patch the running workload now.</small>
          </button>
          <button
            className={`delivery-card ${delivery === "iac" ? "active" : ""}`}
            type="button"
            onClick={() => {
              setDelivery("iac");
              setOutcome(null);
            }}
          >
            <GitPullRequest size={16} />
            <span>Open IaC PR</span>
            <small>Create a reviewable source change.</small>
          </button>
        </div>

        {delivery === "direct" ? (
          <>
            <div className="seg-row self-start">
              {MODES.map((m) => (
                <button
                  key={m.key}
                  className={`seg ${mode === m.key ? "active" : ""}`}
                  onClick={() => setMode(m.key)}
                  type="button"
                >
                  {m.label}
                </button>
              ))}
            </div>

            {mode === "approved" && !authEnabled && (
              <label className="field">
                Actor (required for approved mode)
                <input
                  className="text"
                  type="text"
                  placeholder="operator name, e.g. alice"
                  value={actor}
                  onChange={(e) => setActor(e.target.value)}
                  autoFocus
                />
              </label>
            )}
            {mode === "approved" && authEnabled && (
              <p className="muted-text">Approving as you — the audit trail records your session identity.</p>
            )}
          </>
        ) : (
          <div className="iac-fields">
            <label className="field">
              Repository override
              <input
                className="text"
                type="text"
                placeholder="Leave blank to use saved default"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
              />
              <span className="field-hint">Use only when this recommendation should target a different configured repo or alias.</span>
            </label>
            <details className="advanced-fields">
              <summary>Advanced target</summary>
              <div className="iac-fields nested">
                <label className="field">
                  Source file path
                  <input
                    className="text"
                    type="text"
                    placeholder={isDB ? "terraform/databases.tf" : "kubernetes/apps/deployment.yaml"}
                    value={path}
                    onChange={(e) => setPath(e.target.value)}
                  />
                  <span className="field-hint">
                    Use the file that owns the workload: Terraform `.tf`, Kubernetes `.yaml`, or Helm `values.yaml`.
                  </span>
                </label>
                {showResourceTarget ? (
                  <label className="field">
                    Terraform resource
                    <input
                      className="text"
                      type="text"
                      placeholder={defaultResourceTarget}
                      value={resourceTarget}
                      onChange={(e) => setResourceTarget(e.target.value)}
                    />
                    <span className="field-hint">
                      Optional. Use the Terraform address that owns this workload, for example {defaultResourceTarget}.
                    </span>
                  </label>
                ) : (
                  <div className="field-note">
                    YAML target: Consize matches the workload by kind, name, namespace, and container metadata.
                  </div>
                )}
              </div>
            </details>
            {!authEnabled && (
              <label className="field">
                Actor
                <input
                  className="text"
                  type="text"
                  placeholder="operator name, e.g. alice"
                  value={actor}
                  onChange={(e) => setActor(e.target.value)}
                />
              </label>
            )}
          </div>
        )}

        <div className="modal-result">{outcome}</div>

        <div className="modal-foot">
          <button className="btn" onClick={onClose} type="button">
            Close
          </button>
          <button className="btn primary" onClick={run} disabled={busy} type="button">
            {busy ? "Working…" : delivery === "iac" ? "Open PR" : "Run apply"}
          </button>
        </div>
      </div>
    </div>
  );
}

function formatGuardrailReason(raw: string): { title?: string; message: string } {
  const statusMatch = raw.match(/^recommendation status is "([^"]+)", not pending$/i);
  if (statusMatch) {
    const status = statusMatch[1];
    if (status === "applied") {
      return {
        title: "Already applied",
        message:
          "This recommendation has already been applied. Refresh the list or review the workload history for its verification status.",
      };
    }
    if (status === "superseded") {
      return {
        title: "Recommendation no longer current",
        message: "A newer recommendation replaced this one. Refresh the list and apply the current recommendation instead.",
      };
    }
    return {
      title: "Recommendation unavailable",
      message: `This recommendation is ${status}. Only pending recommendations can be applied.`,
    };
  }
  return { message: formatErrorMessage(raw) };
}

function formatErrorMessage(raw: string): string {
  const text = raw.trim();
  if (!text) return "Something went wrong.";
  return `${text.charAt(0).toUpperCase()}${text.slice(1)}${/[.!?]$/.test(text) ? "" : "."}`;
}

function looksLikeURL(value: string): boolean {
  const v = value.trim().toLowerCase();
  return v.startsWith("http://") || v.startsWith("https://") || v.startsWith("git@");
}

function sourcePathKind(value: string): "terraform" | "yaml" | "helm-values" | "unknown" {
  const v = value.trim().toLowerCase();
  if (!v) return "unknown";
  if (v.endsWith(".tf") || v.endsWith(".tf.json")) return "terraform";
  if ((v.endsWith(".yaml") || v.endsWith(".yml")) && v.includes("values.")) return "helm-values";
  if (v.endsWith(".yaml") || v.endsWith(".yml")) return "yaml";
  return "unknown";
}

function shouldSendTerraformTarget(path: string): boolean {
  const kind = sourcePathKind(path);
  return kind === "terraform" || kind === "unknown";
}

function defaultRecommendationResourceTarget(rec: Recommendation): string {
  const name = safeTerraformName(rec.WorkloadName);
  if (rec.Resource === "class") return `google_sql_database_instance.${name}`;
  return `kubernetes_deployment.${name}`;
}

function safeTerraformName(value: string): string {
  const normalized = value.trim().toLowerCase().replace(/[^a-z0-9_]+/g, "_").replace(/^_+|_+$/g, "");
  return normalized || "main";
}

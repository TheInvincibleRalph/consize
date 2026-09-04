"use client";

import React from "react";
import { AlertTriangle, GitBranch, Plus, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import type { GitHubIntegrationConfig, GitHubRepository } from "@/lib/types";
import { EmptyState, ErrorState, LoadingState, PageHead, Pill } from "@/components/ui";
import { useAuth } from "@/components/auth";

const defaultConfig: GitHubIntegrationConfig = {
  enabled: false,
  organization: "",
  token_env: "CONSIZE_GITHUB_TOKEN",
  default_repo: "",
  default_path: "",
  default_terraform_addr: "",
  repositories: [],
};

function cloneConfig(config: GitHubIntegrationConfig): GitHubIntegrationConfig {
  return {
    enabled: !!config.enabled,
    organization: config.organization || "",
    token_env: config.token_env || "CONSIZE_GITHUB_TOKEN",
    default_repo: config.default_repo || "",
    default_path: config.default_path || "",
    default_terraform_addr: config.default_terraform_addr || "",
    repositories: (config.repositories || []).map((repo) => ({
      alias: repo.alias || "",
      repo: repo.repo || "",
      default_branch: repo.default_branch || "main",
      root_path: repo.root_path || "",
    })),
  };
}

function errMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const body = err.body as { error?: string } | null;
    return body?.error ? `${err.message}: ${body.error}` : err.message;
  }
  return String((err as Error)?.message || err);
}

function repoTemplate(index: number): GitHubRepository {
  return {
    alias: index === 0 ? "cluster" : `repo-${index + 1}`,
    repo: "",
    default_branch: "main",
    root_path: "",
  };
}

export default function IntegrationsView() {
  const { authEnabled, user } = useAuth();
  const canWrite = !authEnabled || user?.role === "admin";
  const [phase, setPhase] = React.useState<"loading" | "ready" | "error">("loading");
  const [source, setSource] = React.useState<string>("default");
  const [tokenPresent, setTokenPresent] = React.useState(false);
  const [config, setConfig] = React.useState<GitHubIntegrationConfig>(defaultConfig);
  const [saving, setSaving] = React.useState(false);
  const [message, setMessage] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .githubIntegration()
      .then((body) => {
        if (!alive) return;
        setConfig(cloneConfig(body.config || defaultConfig));
        setSource(body.source);
        setTokenPresent(!!body.token_present);
        setPhase("ready");
      })
      .catch(() => alive && setPhase("error"));
    return () => {
      alive = false;
    };
  }, []);

  const updateConfig = (patch: Partial<GitHubIntegrationConfig>) => {
    setConfig((current) => ({ ...cloneConfig(current), ...patch }));
  };

  const updateRepo = (index: number, patch: Partial<GitHubRepository>) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.repositories[index] = { ...copy.repositories[index], ...patch };
      return copy;
    });
  };

  const addRepo = () => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.repositories.push(repoTemplate(copy.repositories.length));
      return copy;
    });
  };

  const removeRepo = (index: number) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.repositories = copy.repositories.filter((_, i) => i !== index);
      return copy;
    });
  };

  const save = async () => {
    setSaving(true);
    setMessage("");
    try {
      const body = await api.saveGitHubIntegration(config);
      setConfig(cloneConfig(body.config));
      setSource(body.source);
      setTokenPresent(!!body.token_present);
      setMessage("GitHub integration saved.");
    } catch (err) {
      setMessage(errMessage(err));
    } finally {
      setSaving(false);
    }
  };

  if (phase === "loading") return <LoadingState msg="Loading integrations…" />;
  if (phase === "error") return <ErrorState msg="Integrations failed to load." />;

  return (
    <div>
      <PageHead title="Integrations" sub="Connect source control and delivery systems." />

      {!canWrite && (
        <div className="alerting-callout warning mb-4">
          <AlertTriangle size={15} />
          You can inspect integrations, but only admins can save changes.
        </div>
      )}

      <section className="card integration-summary">
        <div className="integration-provider">
          <div className="integration-icon">
            <GitBranch size={20} />
          </div>
          <div>
            <h2 className="card-title">GitHub</h2>
            <p>Repository access for infrastructure-as-code pull requests.</p>
          </div>
        </div>
        <div className="integration-status">
          <Pill color={config.enabled ? "green" : "gray"}>{config.enabled ? "Enabled" : "Disabled"}</Pill>
          <Pill color={tokenPresent ? "green" : "amber"}>{tokenPresent ? "Token ready" : "Token env missing"}</Pill>
          <Pill color={source === "store" ? "green" : "gray"}>{source === "store" ? "Saved" : "Default"}</Pill>
        </div>
      </section>

      <div className="mt-4 grid grid-cols-1 gap-4 xl:grid-cols-[1.2fr_0.8fr]">
        <div className="space-y-4">
          <section className="card alerting-section">
            <div className="card-head">
              <h2 className="card-title">Connection</h2>
            </div>
            <div className="integration-form-grid">
              <label className="config-check compact">
                <input
                  type="checkbox"
                  checked={config.enabled}
                  disabled={!canWrite}
                  onChange={(e) => updateConfig({ enabled: e.target.checked })}
                />
                Enable GitHub PR workflow
              </label>
              <label className="config-field">
                <span>Organization / account</span>
                <input
                  className="config-input"
                  value={config.organization}
                  disabled={!canWrite}
                  onChange={(e) => updateConfig({ organization: e.target.value })}
                  placeholder="TheInvincibleRalph"
                />
              </label>
              <label className="config-field">
                <span>Token env var</span>
                <input
                  className="config-input mono"
                  value={config.token_env}
                  disabled={!canWrite}
                  onChange={(e) => updateConfig({ token_env: e.target.value })}
                  placeholder="CONSIZE_GITHUB_TOKEN"
                />
              </label>
              <label className="config-field">
                <span>Default repo</span>
                <input
                  className="config-input"
                  value={config.default_repo}
                  disabled={!canWrite}
                  onChange={(e) => updateConfig({ default_repo: e.target.value })}
                  placeholder="org/infra-live"
                />
              </label>
              <label className="config-field">
                <span>Default source path</span>
                <input
                  className="config-input mono"
                  value={config.default_path}
                  disabled={!canWrite}
                  onChange={(e) => updateConfig({ default_path: e.target.value })}
                  placeholder="terraform/workloads.tf or kubernetes/apps/deployment.yaml"
                />
                <small>Optional repo-relative file path. If a repo has a root path, enter the file under that root.</small>
              </label>
            </div>
          </section>

          <section className="card alerting-section">
            <div className="card-head alerting-section-head">
              <div>
                <h2 className="card-title">Repositories</h2>
              </div>
              <button className="btn small" type="button" onClick={addRepo} disabled={!canWrite}>
                <Plus size={14} /> Add repo
              </button>
            </div>
            {config.repositories.length === 0 ? (
              <EmptyState msg="No repositories configured." />
            ) : (
              <div className="alerting-list">
                {config.repositories.map((repo, index) => (
                  <article className="alerting-card" key={`${repo.alias}-${index}`}>
                    <div className="alerting-card-head">
                      <div className="repo-card-heading">
                        <div className="integration-icon small">
                          <GitBranch size={14} />
                        </div>
                        <div>
                          <h3>Repository {index + 1}</h3>
                          <p>Authorized source repo for IaC pull requests.</p>
                        </div>
                      </div>
                      <button className="btn small" type="button" onClick={() => removeRepo(index)} disabled={!canWrite}>
                        <Trash2 size={13} /> Remove
                      </button>
                    </div>
                    <div className="integration-form-grid">
                      <label className="config-field">
                        <span>Alias</span>
                        <input
                          className="config-input"
                          value={repo.alias || ""}
                          disabled={!canWrite}
                          onChange={(e) => updateRepo(index, { alias: e.target.value })}
                          placeholder="cluster"
                        />
                        <small>Short name operators can use instead of owner/repo.</small>
                      </label>
                      <label className="config-field">
                        <span>Repository</span>
                        <input
                          className="config-input"
                          value={repo.repo}
                          disabled={!canWrite}
                          onChange={(e) => updateRepo(index, { repo: e.target.value })}
                          placeholder="org/infra-live"
                        />
                      </label>
                      <label className="config-field">
                        <span>Base branch</span>
                        <input
                          className="config-input"
                          value={repo.default_branch || ""}
                          disabled={!canWrite}
                          onChange={(e) => updateRepo(index, { default_branch: e.target.value })}
                          placeholder="main"
                        />
                      </label>
                      <label className="config-field integration-form-wide">
                        <span>Root path</span>
                        <input
                          className="config-input mono"
                          value={repo.root_path || ""}
                          disabled={!canWrite}
                          onChange={(e) => updateRepo(index, { root_path: e.target.value })}
                          placeholder="infra/live"
                        />
                        <small>Optional folder for monorepos, for example `infra/terraform`.</small>
                      </label>
                    </div>
                  </article>
                ))}
              </div>
            )}
          </section>
        </div>

        <aside className="space-y-4">
          <section className="card alerting-section">
            <div className="card-head">
              <h2 className="card-title">Actions</h2>
            </div>
            <div className="alerting-actions">
              <button className="btn primary" type="button" onClick={save} disabled={!canWrite || saving}>
                {saving ? "Saving…" : "Save integration"}
              </button>
              {message && <p className="alerting-message">{message}</p>}
            </div>
          </section>

          <section className="card alerting-section">
            <div className="card-head">
              <h2 className="card-title">Scope</h2>
            </div>
            <div className="alerting-copy">
              <p>Use a scoped GitHub token or app installation with repository read/write and pull-request access.</p>
              <p>Consize stores repo metadata only. The token itself stays in the API environment.</p>
              <p>Recommendation PRs can use these defaults, or an operator can override the repo and source file at review time.</p>
            </div>
          </section>
        </aside>
      </div>
    </div>
  );
}

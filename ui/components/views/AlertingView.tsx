"use client";

import React from "react";
import { AlertTriangle, CheckCircle2, Plus, Send, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import type {
  AlertContactPoint,
  AlertDeliveryResult,
  AlertNotificationPolicy,
  AlertRoutingConfig,
} from "@/lib/types";
import { useAuth } from "@/components/auth";
import { EmptyState, ErrorState, LoadingState, PageHead, Pill } from "@/components/ui";

const defaultConfig: AlertRoutingConfig = {
  default_contact_point: "ops-slack",
  contact_points: [
    {
      name: "ops-slack",
      integrations: [
        {
          type: "slack",
          webhook_env: "CONSIZE_SLACK_WEBHOOK",
          channel: "#platform-oncall",
          mention: "",
        },
      ],
    },
  ],
  notification_policies: [
    {
      name: "critical-verification",
      match: { severity: "critical" },
      contact_point: "ops-slack",
      continue: false,
    },
  ],
};

function cloneConfig(config: AlertRoutingConfig): AlertRoutingConfig {
  return {
    default_contact_point: config.default_contact_point || "",
    contact_points: (config.contact_points || []).map((cp) => ({
      name: cp.name || "",
      integrations: (cp.integrations || []).map((integration) => ({
        type: integration.type || "slack",
        webhook_env: integration.webhook_env || "",
        channel: integration.channel || "",
        mention: integration.mention || "",
      })),
    })),
    notification_policies: (config.notification_policies || []).map((policy) => ({
      name: policy.name || "",
      match: { ...(policy.match || {}) },
      contact_point: policy.contact_point || "",
      continue: !!policy.continue,
    })),
  };
}

function isEmptyConfig(config: AlertRoutingConfig): boolean {
  return !config.default_contact_point && !config.contact_points?.length && !config.notification_policies?.length;
}

function matcherText(match: Record<string, string>): string {
  return Object.entries(match || {})
    .map(([key, value]) => `${key}=${value}`)
    .join("\n");
}

function parseMatcherText(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const i = trimmed.indexOf("=");
    if (i <= 0) continue;
    const key = trimmed.slice(0, i).trim();
    const value = trimmed.slice(i + 1).trim();
    if (key && value) out[key] = value;
  }
  return out;
}

function errMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const body = err.body as { error?: string } | null;
    return body?.error ? `${err.message}: ${body.error}` : err.message;
  }
  return String((err as Error)?.message || err);
}

function contactPointTemplate(index: number): AlertContactPoint {
  return {
    name: index === 0 ? "ops-slack" : `slack-${index + 1}`,
    integrations: [{ type: "slack", webhook_env: "CONSIZE_SLACK_WEBHOOK", channel: "", mention: "" }],
  };
}

function policyTemplate(contactPoint: string, index: number): AlertNotificationPolicy {
  return {
    name: index === 0 ? "critical-verification" : `policy-${index + 1}`,
    match: index === 0 ? { severity: "critical" } : {},
    contact_point: contactPoint,
    continue: false,
  };
}

function TestResults({ results }: { results: AlertDeliveryResult[] | null }) {
  if (!results) return null;
  if (results.length === 0) {
    return (
      <div className="alerting-callout warning">
        <AlertTriangle size={15} />
        No route matched the test event. Add a default contact point or a policy matching <code>severity=critical</code>.
      </div>
    );
  }
  return (
    <div className="alerting-results">
      {results.map((result, i) => (
        <div className={`alerting-result ${result.status === "sent" ? "ok" : "bad"}`} key={`${result.contact_point}-${i}`}>
          {result.status === "sent" ? <CheckCircle2 size={15} /> : <AlertTriangle size={15} />}
          <span className="mono">{result.contact_point}</span>
          <span>{result.type}</span>
          <span>{result.status}</span>
          {result.error && <span className="alerting-result-error">{result.error}</span>}
        </div>
      ))}
    </div>
  );
}

export default function AlertingView() {
  const { authEnabled, user } = useAuth();
  const canWrite = !authEnabled || user?.role === "admin";
  const [phase, setPhase] = React.useState<"loading" | "ready" | "error">("loading");
  const [source, setSource] = React.useState<string>("store");
  const [config, setConfig] = React.useState<AlertRoutingConfig>(defaultConfig);
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [message, setMessage] = React.useState<string>("");
  const [testResults, setTestResults] = React.useState<AlertDeliveryResult[] | null>(null);

  React.useEffect(() => {
    let alive = true;
    api
      .alertingConfig()
      .then((body) => {
        if (!alive) return;
        setSource(body.source);
        setConfig(isEmptyConfig(body.config) ? cloneConfig(defaultConfig) : cloneConfig(body.config));
        setPhase("ready");
      })
      .catch(() => alive && setPhase("error"));
    return () => {
      alive = false;
    };
  }, []);

  const contactNames = config.contact_points.map((cp) => cp.name).filter(Boolean);

  const updateContactPoint = (index: number, next: Partial<AlertContactPoint>) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.contact_points[index] = { ...copy.contact_points[index], ...next };
      return copy;
    });
  };

  const updateIntegration = (index: number, next: Partial<AlertContactPoint["integrations"][number]>) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      const currentIntegration = copy.contact_points[index]?.integrations?.[0] || { type: "slack", webhook_env: "" };
      copy.contact_points[index].integrations = [{ ...currentIntegration, ...next, type: "slack" }];
      return copy;
    });
  };

  const updatePolicy = (index: number, next: Partial<AlertNotificationPolicy>) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.notification_policies[index] = { ...copy.notification_policies[index], ...next };
      return copy;
    });
  };

  const addContactPoint = () => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.contact_points.push(contactPointTemplate(copy.contact_points.length));
      if (!copy.default_contact_point) copy.default_contact_point = copy.contact_points[0].name;
      return copy;
    });
  };

  const removeContactPoint = (index: number) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      const removed = copy.contact_points[index]?.name;
      copy.contact_points = copy.contact_points.filter((_, i) => i !== index);
      if (copy.default_contact_point === removed) copy.default_contact_point = copy.contact_points[0]?.name || "";
      copy.notification_policies = copy.notification_policies.map((policy) =>
        policy.contact_point === removed ? { ...policy, contact_point: copy.default_contact_point } : policy,
      );
      return copy;
    });
  };

  const addPolicy = () => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.notification_policies.push(policyTemplate(copy.default_contact_point || copy.contact_points[0]?.name || "", copy.notification_policies.length));
      return copy;
    });
  };

  const removePolicy = (index: number) => {
    setConfig((current) => {
      const copy = cloneConfig(current);
      copy.notification_policies = copy.notification_policies.filter((_, i) => i !== index);
      return copy;
    });
  };

  const save = async () => {
    setSaving(true);
    setMessage("");
    setTestResults(null);
    try {
      const body = await api.saveAlertingConfig(config);
      setConfig(cloneConfig(body.config));
      setSource(body.source);
      setMessage("Alerting configuration saved.");
    } catch (err) {
      setMessage(errMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const test = async () => {
    setTesting(true);
    setMessage("");
    setTestResults(null);
    try {
      const body = await api.testAlerting(config);
      const results = body.results || [];
      setTestResults(results);
      const failed = results.filter((result) => result.status === "failed");
      if (results.length === 0) {
        setMessage("No contact point matched this test alert.");
      } else if (failed.length > 0) {
        setMessage("Test finished, but one or more deliveries failed.");
      } else {
        setMessage("Test notification sent.");
      }
    } catch (err) {
      const apiErr = err instanceof ApiError ? (err.body as { results?: AlertDeliveryResult[] } | null) : null;
      if (apiErr?.results) setTestResults(apiErr.results);
      setMessage(errMessage(err));
    } finally {
      setTesting(false);
    }
  };

  if (phase === "loading") return <LoadingState msg="Loading alerting configuration…" />;
  if (phase === "error") return <ErrorState msg="Alerting configuration failed to load." />;

  return (
    <div>
      <PageHead
        title="Alerting"
        sub="Contact points and notification policies."
      />

      <section className="card alerting-summary">
        <div className="alerting-summary-main">
          <div>
            <h2 className="card-title">Notification routing</h2>
          </div>
          <Pill color={source === "store" ? "green" : "gray"}>{source === "store" ? "Configured" : "Provisioned"}</Pill>
        </div>

        <div className="alerting-summary-grid">
          <div>
            <span>Contact points</span>
            <strong>{config.contact_points.length}</strong>
          </div>
          <div>
            <span>Policies</span>
            <strong>{config.notification_policies.length}</strong>
          </div>
          <div>
            <span>Default route</span>
            <strong>{config.default_contact_point || "Not set"}</strong>
          </div>
        </div>
      </section>

      {!canWrite && (
        <div className="alerting-callout warning mt-4">
          <AlertTriangle size={15} />
          You can inspect alerting, but only admins can save or test notification routing.
        </div>
      )}

      <div className="mt-4 grid grid-cols-1 gap-4 xl:grid-cols-[1.25fr_0.75fr]">
        <div className="space-y-4">
          <section className="card alerting-section">
            <div className="card-head alerting-section-head">
              <div>
                <h2 className="card-title">Contact points</h2>
              </div>
              <button className="btn small" type="button" onClick={addContactPoint} disabled={!canWrite}>
                <Plus size={14} /> Add contact point
              </button>
            </div>

            {config.contact_points.length === 0 ? (
              <EmptyState msg="No contact points configured." />
            ) : (
              <div className="alerting-list">
                {config.contact_points.map((cp, index) => {
                  const integration = cp.integrations[0] || { type: "slack", webhook_env: "" };
                  return (
                    <article className="alerting-card" key={`${cp.name}-${index}`}>
                      <div className="alerting-card-head">
                        <div>
                          <div className="micro">Slack contact point</div>
                          <input
                            className="config-input title"
                            value={cp.name}
                            disabled={!canWrite}
                            onChange={(e) => updateContactPoint(index, { name: e.target.value })}
                            aria-label="Contact point name"
                          />
                        </div>
                        <button className="btn small" type="button" onClick={() => removeContactPoint(index)} disabled={!canWrite || config.contact_points.length === 1}>
                          <Trash2 size={13} /> Remove
                        </button>
                      </div>
                      <div className="alerting-form-grid">
                        <label className="config-field">
                          <span>Webhook env var</span>
                          <input
                            className="config-input mono"
                            value={integration.webhook_env || ""}
                            disabled={!canWrite}
                            onChange={(e) => updateIntegration(index, { webhook_env: e.target.value })}
                            placeholder="CONSIZE_SLACK_WEBHOOK"
                          />
                        </label>
                        <label className="config-field">
                          <span>Channel label</span>
                          <input
                            className="config-input"
                            value={integration.channel || ""}
                            disabled={!canWrite}
                            onChange={(e) => updateIntegration(index, { channel: e.target.value })}
                            placeholder="#platform-oncall"
                          />
                        </label>
                        <label className="config-field alerting-form-wide">
                          <span>On-call mention</span>
                          <input
                            className="config-input mono"
                            value={integration.mention || ""}
                            disabled={!canWrite}
                            onChange={(e) => updateIntegration(index, { mention: e.target.value })}
                            placeholder="<!subteam^S123456> or <@U123456>"
                          />
                        </label>
                      </div>
                    </article>
                  );
                })}
              </div>
            )}
          </section>

          <section className="card alerting-section">
            <div className="card-head alerting-section-head">
              <div>
                <h2 className="card-title">Notification policies</h2>
              </div>
              <button className="btn small" type="button" onClick={addPolicy} disabled={!canWrite}>
                <Plus size={14} /> Add policy
              </button>
            </div>

            <div className="alerting-list">
              <label className="config-field">
                <span>Default contact point</span>
                <select
                  className="config-input"
                  value={config.default_contact_point}
                  disabled={!canWrite}
                  onChange={(e) => setConfig((current) => ({ ...cloneConfig(current), default_contact_point: e.target.value }))}
                >
                  <option value="">None</option>
                  {contactNames.map((name) => (
                    <option value={name} key={name}>{name}</option>
                  ))}
                </select>
              </label>

              {config.notification_policies.map((policy, index) => (
                <article className="alerting-card" key={`${policy.name}-${index}`}>
                  <div className="alerting-card-head">
                    <input
                      className="config-input title"
                      value={policy.name}
                      disabled={!canWrite}
                      onChange={(e) => updatePolicy(index, { name: e.target.value })}
                      aria-label="Policy name"
                    />
                    <button className="btn small" type="button" onClick={() => removePolicy(index)} disabled={!canWrite}>
                      <Trash2 size={13} /> Remove
                    </button>
                  </div>
                  <div className="alerting-form-grid">
                    <label className="config-field">
                      <span>Route to</span>
                      <select
                        className="config-input"
                        value={policy.contact_point}
                        disabled={!canWrite}
                        onChange={(e) => updatePolicy(index, { contact_point: e.target.value })}
                      >
                        <option value="">Select contact point</option>
                        {contactNames.map((name) => (
                          <option value={name} key={name}>{name}</option>
                        ))}
                      </select>
                    </label>
                    <label className="config-check">
                      <input
                        type="checkbox"
                        checked={!!policy.continue}
                        disabled={!canWrite}
                        onChange={(e) => updatePolicy(index, { continue: e.target.checked })}
                      />
                      Continue matching sibling policies
                    </label>
                    <label className="config-field alerting-form-wide">
                      <span>Label matchers</span>
                      <textarea
                        className="config-textarea mono"
                        value={matcherText(policy.match)}
                        disabled={!canWrite}
                        onChange={(e) => updatePolicy(index, { match: parseMatcherText(e.target.value) })}
                        placeholder={"severity=critical\nalertname=ConsizeVerificationFailed"}
                      />
                    </label>
                  </div>
                </article>
              ))}
            </div>
          </section>
        </div>

        <aside className="space-y-4">
          <section className="card alerting-section">
            <div className="card-head">
              <h2 className="card-title">Actions</h2>
            </div>
            <div className="alerting-actions">
              <button className="btn primary" type="button" onClick={save} disabled={!canWrite || saving}>
                {saving ? "Saving…" : "Save routing"}
              </button>
              <button className="btn" type="button" onClick={test} disabled={!canWrite || testing}>
                <Send size={14} /> {testing ? "Sending…" : "Send test"}
              </button>
              {message && <p className="alerting-message">{message}</p>}
              <TestResults results={testResults} />
            </div>
          </section>

          <section className="card alerting-section">
            <div className="card-head">
              <h2 className="card-title">Setup notes</h2>
            </div>
            <div className="alerting-copy">
              <p>
                Create the Slack webhook as a Kubernetes Secret, expose it as <code>CONSIZE_SLACK_WEBHOOK</code>, then reference that env var here.
              </p>
              <p>
                Incoming webhooks are channel-bound in Slack. The channel field here is a human label for audits and previews.
              </p>
            </div>
          </section>
        </aside>
      </div>
    </div>
  );
}

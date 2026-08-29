"use client";

import Link from "next/link";
import React from "react";

/* ---------- pills ---------- */

const STATUS_COLOR: Record<string, string> = {
  pending: "blue",
  applied: "green",
  verified: "teal",
  rolled_back: "red",
  superseded: "gray",
  rejected: "red",
};
const RESULT_COLOR: Record<string, string> = {
  planned: "gray",
  applied: "green",
  reverted: "red",
};
const VERDICT_COLOR: Record<string, string> = {
  passed: "green",
  failed: "red",
  inconclusive: "amber",
};
const RISK_COLOR: Record<string, string> = {
  low: "green",
  medium: "amber",
  high: "red",
};

export function Pill({
  color,
  children,
  title,
}: {
  color: string;
  children: React.ReactNode;
  title?: string;
}) {
  return (
    <span className={`pill ${color}`} title={title}>
      {children}
    </span>
  );
}

export function statusPill(status?: string) {
  const s = status || "—";
  return <Pill color={STATUS_COLOR[s] || "gray"}>{s}</Pill>;
}

export function resultPill(result?: string) {
  const r = result || "—";
  return <Pill color={RESULT_COLOR[r] || "gray"}>{r}</Pill>;
}

export function verdictPill(verdict?: string) {
  const v = verdict || "—";
  return <Pill color={VERDICT_COLOR[v] || "gray"}>{v}</Pill>;
}

/* Risk pill: low|medium|high from Recommendation.Risk; RiskReasons rides
 * the native title tooltip. The backend marshals snake_case — both cases
 * are handled by pick() upstream. */
export function riskPill(risk?: string, reasons?: string[] | string) {
  const text = risk == null || risk === "" ? "n/a" : String(risk);
  const title = Array.isArray(reasons)
    ? reasons.join("\n")
    : typeof reasons === "string"
      ? reasons
      : undefined;
  return <Pill color={RISK_COLOR[text] || "gray"} title={title}>{text}</Pill>;
}

/* Kind pill: compute (cpu/memory) vs database (class). */
export function kindPill(isDB: boolean) {
  return <Pill color={isDB ? "teal" : "blue"}>{isDB ? "database" : "compute"}</Pill>;
}

export function Chip({ children, cls }: { children: React.ReactNode; cls?: string }) {
  return <span className={`chip ${cls || ""}`}>{children}</span>;
}

/* ---------- delta cell (before → after) ---------- */

export function Delta({ from, to }: { from: string; to: string }) {
  return (
    <span className="delta">
      <code>{from || "—"}</code>
      <span className="arrow">→</span>
      <code>{to || "—"}</code>
    </span>
  );
}

/* ---------- avatar ---------- */

export function Avatar({
  name,
  kind,
  large,
}: {
  name: string;
  kind: "k8s" | "db";
  large?: boolean;
}) {
  const initial = String(name || "?").charAt(0).toUpperCase();
  return <span className={`avatar ${kind} ${large ? "lg" : ""}`}>{initial}</span>;
}

export function WorkloadCell({
  name,
  namespace,
  id,
  isDB,
}: {
  name: string;
  namespace: string;
  id: number;
  isDB: boolean;
}) {
  return (
    <div className="wl-cell">
      <Avatar name={name} kind={isDB ? "db" : "k8s"} />
      <Link href={`/workloads/${id}`}>
        <div className="wl">
          <strong>{name}</strong>
          <span>{namespace}</span>
        </div>
      </Link>
    </div>
  );
}

/* ---------- states ---------- */

export function LoadingState({ msg }: { msg: string }) {
  return (
    <div className="state">
      <div className="spinner" />
      <p className="state-sub">{msg}</p>
    </div>
  );
}

export function EmptyState({ msg }: { msg: string }) {
  return (
    <div className="state">
      <p className="state-title">{msg}</p>
    </div>
  );
}

export function ErrorState({ msg }: { msg: string }) {
  return (
    <div className="state">
      <p className="state-title">API unreachable — start the API and reload</p>
      <p className="state-sub">{msg}</p>
      <button className="btn mt-2" onClick={() => location.reload()}>
        Reload
      </button>
    </div>
  );
}

/* A 404 is not an API failure — no "API unreachable" framing. */
export function NotFoundState({
  title,
  sub,
  href,
  linkLabel,
}: {
  title: string;
  sub: string;
  href: string;
  linkLabel: string;
}) {
  return (
    <div className="state">
      <p className="state-title">{title}</p>
      <p className="state-sub">{sub}</p>
      <Link className="btn mt-2" href={href}>
        {linkLabel}
      </Link>
    </div>
  );
}

export function PageHead({ title, sub }: { title: string; sub?: string }) {
  return (
    <div className="page-head">
      <div className="page-head-row">
        <h1>{title}</h1>
        {sub && <p className="sub">{sub}</p>}
      </div>
    </div>
  );
}

/* ---------- table scaffolding ---------- */

export function Table({
  head,
  children,
}: {
  head: { label: string; right?: boolean }[];
  children: React.ReactNode;
}) {
  return (
    <div className="tbl-wrap">
      <table className="data">
        <thead>
          <tr>
            {head.map((h) => (
              <th key={h.label} className={h.right ? "num" : ""} scope="col">
                {h.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}

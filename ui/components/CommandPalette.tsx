"use client";

import React from "react";
import { useRouter } from "next/navigation";
import { BellRing, CloudOff, FileText, LayoutDashboard, ListChecks, Boxes, ScrollText, Search, Plug } from "lucide-react";
import { api } from "@/lib/api";
import type { Workload } from "@/lib/types";

interface Item {
  id: string;
  label: string;
  hint: string;
  href: string;
  icon: React.ReactNode;
}

const ROUTES: Item[] = [
  { id: "route-0", label: "Dashboard", hint: "overview", href: "/", icon: <LayoutDashboard size={14} /> },
  { id: "route-1", label: "Recommendations", hint: "decisions", href: "/recommendations", icon: <ListChecks size={14} /> },
  { id: "route-2", label: "Workloads", hint: "resources", href: "/workloads", icon: <Boxes size={14} /> },
  { id: "route-3", label: "Cloud waste", hint: "orphaned cost", href: "/cost", icon: <CloudOff size={14} /> },
  { id: "route-4", label: "Alerting", hint: "contact points", href: "/alerting", icon: <BellRing size={14} /> },
  { id: "route-5", label: "Integrations", hint: "github", href: "/integrations", icon: <Plug size={14} /> },
  { id: "route-6", label: "Reports", hint: "weekly digest", href: "/reports", icon: <FileText size={14} /> },
  { id: "route-7", label: "Audit", hint: "apply trail", href: "/audit", icon: <ScrollText size={14} /> },
];

const surfaceHint = (w: Workload): string => {
  const s = (w.Source || "").toLowerCase();
  if (s === "db") return w.DBProvider ? `database · ${w.DBProvider}` : "database";
  return `compute · ${w.Namespace}`;
};

export function CommandPalette() {
  const router = useRouter();
  const [open, setOpen] = React.useState(false);
  const [q, setQ] = React.useState("");
  const [idx, setIdx] = React.useState(0);
  const [workloads, setWorkloads] = React.useState<Workload[] | null>(null);
  const inputRef = React.useRef<HTMLInputElement>(null);

  const openPalette = React.useCallback(() => {
    setQ("");
    setIdx(0);
    setOpen(true);
  }, []);

  
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        if (open) setOpen(false);
        else openPalette();
      } else if (e.key === "Escape") {
        setOpen(false);
      }
    };
    const onOpen = () => openPalette();
    window.addEventListener("keydown", onKey);
    window.addEventListener("consize:open-command-palette", onOpen);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("consize:open-command-palette", onOpen);
    };
  }, [open, openPalette]);

  
  React.useEffect(() => {
    if (!open) return;
    if (workloads === null) {
      api
        .workloads()
        .then(setWorkloads)
        .catch(() => setWorkloads([])); // palette degrades to routes-only
    }
    const t = window.setTimeout(() => inputRef.current?.focus(), 30);
    return () => window.clearTimeout(t);
  }, [open, workloads]);

  const filtered = React.useMemo<Item[]>(() => {
    const t = q.trim().toLowerCase();
    if (!t) return ROUTES;
    const routes = ROUTES.filter((r) => r.label.toLowerCase().includes(t));
    const ws = (workloads ?? [])
      .filter((w) => (w.Name || "").toLowerCase().includes(t))
      .slice(0, 8)
      .map<Item>((w) => ({
        id: "workload-" + w.ID,
        label: w.Name,
        hint: surfaceHint(w),
        href: `/workloads/${w.ID}`,
        icon: (
          <span className={`avatar sm ${(w.Source || "").toLowerCase() === "db" ? "db" : "k8s"}`}>
            {(w.Name || "?").slice(0, 1).toUpperCase()}
          </span>
        ),
      }));
    return [...routes, ...ws];
  }, [q, workloads]);

  const jump = (item?: Item) => {
    if (!item) return;
    setOpen(false);
    router.push(item.href);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setIdx((i) => Math.min(i + 1, filtered.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setIdx((i) => Math.max(i - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      jump(filtered[idx]);
    }
  };

  if (!open) return null;

  return (
    <div className="modal-overlay" role="presentation" onMouseDown={(e) => e.target === e.currentTarget && setOpen(false)}>
      <div className="palette" role="dialog" aria-modal="true" aria-label="Jump to">
        <div className="flex items-center gap-2 border-b border-edge px-4">
          <Search size={15} className="shrink-0 text-faint" />
          <input
            ref={inputRef}
            value={q}
            onChange={(e) => {
              setQ(e.target.value);
              setIdx(0);
            }}
            onKeyDown={onKeyDown}
            placeholder="Jump to a page or workload…"
            aria-label="Search pages and workloads"
            className="palette-input"
          />
        </div>
        <div className="palette-list" role="listbox">
          {filtered.length === 0 && (
            <div className="px-4 py-6 text-center text-[12.5px] text-faint">No matches for “{q}”</div>
          )}
          {filtered.map((item, i) => (
            <button
              key={item.id}
              type="button"
              role="option"
              aria-selected={i === idx}
              className={`palette-item ${i === idx ? "active" : ""}`}
              onMouseEnter={() => setIdx(i)}
              onClick={() => jump(item)}
            >
              <span className="flex h-5 w-5 items-center justify-center text-faint">{item.icon}</span>
              <span className="truncate">{item.label}</span>
              <span className="hint truncate">{item.hint}</span>
            </button>
          ))}
        </div>
        <div className="palette-foot">
          <span>
            <kbd>↑</kbd> <kbd>↓</kbd> navigate
          </span>
          <span>
            <kbd>↵</kbd> jump
          </span>
          <span>
            <kbd>esc</kbd> close
          </span>
          <span className="ml-auto text-faint">⌘K anywhere</span>
        </div>
      </div>
    </div>
  );
}

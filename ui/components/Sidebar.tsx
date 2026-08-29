"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { BellRing, CloudOff, FileText, LayoutDashboard, ListChecks, Boxes, ScrollText, LogOut, Plug } from "lucide-react";
import { useAuth } from "@/components/auth";
import { Brand } from "@/components/Brand";

const NAV: { section: string; items: { href: string; label: string; icon: typeof LayoutDashboard }[] }[] = [
  {
    section: "Overview",
    items: [{ href: "/", label: "Dashboard", icon: LayoutDashboard }],
  },
  {
    section: "Optimize",
    items: [
      { href: "/recommendations", label: "Recommendations", icon: ListChecks },
      { href: "/workloads", label: "Workloads", icon: Boxes },
      { href: "/cost", label: "Cloud waste", icon: CloudOff },
    ],
  },
  {
    section: "Operations",
    items: [
      { href: "/alerting", label: "Alerting", icon: BellRing },
      { href: "/integrations", label: "Integrations", icon: Plug },
      { href: "/reports", label: "Reports", icon: FileText },
      { href: "/audit", label: "Audit", icon: ScrollText },
    ],
  },
];

export function Sidebar({ open, onClose }: { open: boolean; onClose: () => void }) {
  const pathname = usePathname();
  const { authEnabled, user, logout } = useAuth();

  const isActive = (href: string) => (href === "/" ? pathname === "/" : pathname.startsWith(href));

  return (
    <>
      <aside className={`sidebar ${open ? "translate-x-0" : "-translate-x-full"} lg:translate-x-0`}>
        <div className="px-5 py-5">
          <Brand />
        </div>

        <nav className="mt-1 flex flex-col gap-4" aria-label="Main">
          {NAV.map(({ section, items }) => (
            <div key={section}>
              <div className="nav-section">{section}</div>
              <div className="mt-1 flex flex-col gap-0.5">
                {items.map(({ href, label, icon: Icon }) => {
                  const active = isActive(href);
                  return (
                    <Link
                      key={href}
                      href={href}
                      onClick={onClose}
                      aria-current={active ? "page" : undefined}
                      className={`nav-link ${active ? "active" : ""}`}
                    >
                      <Icon size={15} strokeWidth={1.8} />
                      {label}
                    </Link>
                  );
                })}
              </div>
            </div>
          ))}
        </nav>

        {authEnabled && user && (
          <div className="mt-auto space-y-2 px-5 py-5">
            <div className="sidebar-account">
              <div className="sidebar-avatar">
                {user.email.slice(0, 1).toUpperCase()}
              </div>
              <div className="min-w-0 flex-1">
                <div className="truncate text-[12px] font-semibold text-ink">{user.email}</div>
                <div className="micro capitalize text-muted">{user.role}</div>
              </div>
              <button
                type="button"
                onClick={() => void logout()}
                title="Sign out"
                aria-label="Sign out"
                className="sidebar-icon-btn"
              >
                <LogOut size={14} />
              </button>
            </div>
          </div>
        )}
      </aside>
      
      {open && <div className="nav-scrim fixed inset-0 z-30 backdrop-blur-[2px] lg:hidden" onClick={onClose} aria-hidden />}
    </>
  );
}

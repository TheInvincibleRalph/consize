"use client";

import React from "react";
import { usePathname, useRouter } from "next/navigation";
import { Menu, Search, X } from "lucide-react";
import { Sidebar } from "@/components/Sidebar";
import { Brand } from "@/components/Brand";
import { CommandPalette } from "@/components/CommandPalette";
import { ThemeSeg, ThemeToggle } from "@/components/ThemeToggle";
import { useAuth } from "@/components/auth";

const routeTitle = (pathname: string) => {
  if (pathname === "/") return "Dashboard";
  if (pathname.startsWith("/recommendations")) return "Recommendations";
  if (pathname.startsWith("/workloads")) return "Workloads";
  if (pathname.startsWith("/cost")) return "Cloud waste";
  if (pathname.startsWith("/alerting")) return "Alerting";
  if (pathname.startsWith("/integrations")) return "Integrations";
  if (pathname.startsWith("/reports")) return "Reports";
  if (pathname.startsWith("/audit")) return "Audit";
  return "Workspace";
};

export function Shell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const { status, authEnabled, user } = useAuth();
  const [navOpen, setNavOpen] = React.useState(false);

  const isLogin = pathname === "/login";

  React.useEffect(() => {
    if (status !== "ready" || isLogin) return;
    if (authEnabled && !user) router.replace("/login");
  }, [status, authEnabled, user, isLogin, router]);

  if (isLogin) {
    
    return <>{children}</>;
  }

  if (status === "loading") {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-4">
        <Brand />
        <div className="micro text-muted">Loading…</div>
      </div>
    );
  }

  if (authEnabled && !user) {
    
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-4">
        <Brand />
        <div className="micro text-muted">Signing in…</div>
      </div>
    );
  }

  return (
    <div className="app-shell flex min-h-screen">
      <Sidebar open={navOpen} onClose={() => setNavOpen(false)} />
      <main className="app-main min-w-0 flex-1 pl-0 lg:pl-[232px]">
        
        <div className="mobile-topbar sticky top-0 z-20 flex items-center gap-3 border-b border-edge bg-bgsoft/95 px-4 py-3 backdrop-blur lg:hidden">
          <button
            type="button"
            onClick={() => setNavOpen(true)}
            aria-label="Open navigation"
            className="topbar-icon-btn"
          >
            {navOpen ? <X size={18} /> : <Menu size={18} />}
          </button>
          <Brand />
          <div className="ml-auto">
            <ThemeToggle />
          </div>
        </div>
        <div className="app-topbar hidden lg:flex">
          <div>
            <div className="app-eyebrow">Consize console</div>
            <div className="app-title">{routeTitle(pathname)}</div>
          </div>
          <div className="app-topbar-actions">
            <button
              type="button"
              className="command-trigger"
              aria-label="Open command palette"
              onClick={() => window.dispatchEvent(new CustomEvent("consize:open-command-palette"))}
            >
              <Search size={14} />
              <span>Search</span>
              <kbd>⌘K</kbd>
            </button>
            <ThemeSeg className="theme-switch-compact" />
            {authEnabled && user && (
              <div className="user-chip" title={user.email}>
                <span>{user.email.slice(0, 1).toUpperCase()}</span>
              </div>
            )}
          </div>
        </div>
        <div className="workspace mx-auto w-full max-w-[1320px] px-4 py-5 sm:px-7 sm:py-7 lg:px-9 lg:py-7">{children}</div>
      </main>
      <CommandPalette />
    </div>
  );
}

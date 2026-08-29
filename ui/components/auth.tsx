"use client";

import React from "react";
import { api } from "@/lib/api";
import type { User } from "@/lib/types";

interface AuthState {
  status: "loading" | "ready";
  authEnabled: boolean;
  needsSetup: boolean;
  user: User | null;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  setup: (name: string, email: string, password: string) => Promise<void>;
}

const AuthContext = React.createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = React.useState<"loading" | "ready">("loading");
  const [authEnabled, setAuthEnabled] = React.useState(false);
  const [needsSetup, setNeedsSetup] = React.useState(false);
  const [user, setUser] = React.useState<User | null>(null);

  React.useEffect(() => {
    let alive = true;
    api
      .me()
      .then((me) => {
        if (!alive) return;
        setAuthEnabled(me.auth_enabled);
        setNeedsSetup(!!me.needs_setup);
        setUser(me.user);
      })
      .catch(() => {
        if (!alive) return;
        // Unreachable in practice (me swallows 401), but never strand
        
        
        setAuthEnabled(false);
        setNeedsSetup(false);
        setUser(null);
      })
      .finally(() => alive && setStatus("ready"));
    return () => {
      alive = false;
    };
  }, []);

  const login = React.useCallback(async (email: string, password: string) => {
    const u = await api.login(email, password);
    setAuthEnabled(true);
    setNeedsSetup(false);
    setUser(u);
  }, []);

  const logout = React.useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setUser(null);
    }
  }, []);

  /* First-run wizard: creates the first admin, then signs it in so the
   * session (and the sidebar identity) are real — never a half-logged-in
   * state. */
  const setup = React.useCallback(async (name: string, email: string, password: string) => {
    await api.setup(name, email, password);
    const u = await api.login(email, password);
    setAuthEnabled(true);
    setNeedsSetup(false);
    setUser(u);
  }, []);

  const value = React.useMemo(
    () => ({ status, authEnabled, needsSetup, user, login, logout, setup }),
    [status, authEnabled, needsSetup, user, login, logout, setup],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = React.useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside <AuthProvider>");
  return ctx;
}

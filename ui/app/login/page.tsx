"use client";

import React from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/components/auth";
import { Brand } from "@/components/Brand";
import { ThemeToggle } from "@/components/ThemeToggle";

export default function LoginPage() {
  const router = useRouter();
  const { status, authEnabled, needsSetup, login, setup } = useAuth();
  const [name, setName] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  // Auth disabled build: no login surface exists — go straight to the app.
  React.useEffect(() => {
    if (status === "ready" && !authEnabled) router.replace("/");
  }, [status, authEnabled, router]);

  const wizard = status === "ready" && authEnabled && needsSetup;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || !email || !password) return;
    setBusy(true);
    setError(null);
    try {
      await login(email, password);
      router.replace("/");
    } catch {
      setError("Invalid email or password.");
    } finally {
      setBusy(false);
    }
  };

  const submitSetup = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || !email || password.length < 8) return;
    setBusy(true);
    setError(null);
    try {
      await setup(name, email, password);
      router.replace("/");
    } catch {
      setError("Could not create the admin account. Try again.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden px-6">
      
      <div
        aria-hidden
        className="pointer-events-none absolute left-1/2 top-[-220px] h-[440px] w-[760px] max-w-full -translate-x-1/2 rounded-full bg-[radial-gradient(closest-side,rgba(255,255,255,0.05),transparent)]"
      />
      
      <div className="absolute right-4 top-4">
        <ThemeToggle />
      </div>
      <div className="relative w-full max-w-[380px]">
        <div className="mb-9 flex flex-col items-center gap-4">
          <Brand size="lg" />
          <div className="micro text-muted">
            {wizard ? "First-run setup" : "Sign in to your workspace"}
          </div>
        </div>

        {wizard ? (
          <form onSubmit={submitSetup} className="card space-y-4 p-6">
            <label className="field" htmlFor="name">
              Name <span className="text-faint">(optional)</span>
              <input
                id="name"
                type="text"
                autoComplete="name"
                className="text"
                placeholder="FinOps lead"
                value={name}
                onChange={(e) => setName(e.target.value)}
                disabled={busy}
                autoFocus
              />
            </label>
            <label className="field" htmlFor="email">
              Email
              <input
                id="email"
                type="email"
                autoComplete="username"
                className="text"
                placeholder="you@company.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                disabled={busy}
              />
            </label>
            <label className="field" htmlFor="password">
              Password
              <input
                id="password"
                type="password"
                autoComplete="new-password"
                className="text"
                placeholder="at least 8 characters"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={busy}
              />
            </label>

            {error && (
              <div className="rounded-md border border-[rgba(248,113,113,0.25)] bg-[rgba(248,113,113,0.08)] px-3 py-2 text-[12px] text-red">
                {error}
              </div>
            )}

            <button
              type="submit"
              className="btn primary w-full justify-center"
              disabled={busy || !email || password.length < 8}
            >
              {busy ? "Creating…" : "Create admin & sign in"}
            </button>
          </form>
        ) : (
          <form onSubmit={submit} className="card space-y-4 p-6">
            <label className="field" htmlFor="email">
              Email
              <input
                id="email"
                type="email"
                autoComplete="username"
                className="text"
                placeholder="you@company.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                disabled={busy}
                autoFocus
              />
            </label>
            <label className="field" htmlFor="password">
              Password
              <input
                id="password"
                type="password"
                autoComplete="current-password"
                className="text"
                placeholder="••••••••"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={busy}
              />
            </label>

            {error && (
              <div className="rounded-md border border-[rgba(248,113,113,0.25)] bg-[rgba(248,113,113,0.08)] px-3 py-2 text-[12px] text-red">
                {error}
              </div>
            )}

            <button type="submit" className="btn primary w-full justify-center" disabled={busy || !email || !password}>
              {busy ? "Signing in…" : "Sign in"}
            </button>
          </form>
        )}
      </div>
    </div>
  );
}

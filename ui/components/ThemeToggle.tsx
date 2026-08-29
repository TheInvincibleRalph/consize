"use client";

import { useTheme } from "next-themes";
import { Sun, Moon } from "lucide-react";

export function ThemeToggle({ className = "" }: { className?: string }) {
  const { resolvedTheme, setTheme } = useTheme();
  const dark = resolvedTheme !== "light";
  return (
    <button
      type="button"
      onClick={() => setTheme(dark ? "light" : "dark")}
      aria-label={dark ? "Switch to light mode" : "Switch to dark mode"}
      title={dark ? "Switch to light mode" : "Switch to dark mode"}
      className={`theme-icon-toggle ${className}`}
    >
      {dark ? <Sun size={15} /> : <Moon size={15} />}
    </button>
  );
}

export function ThemeSeg({ className = "" }: { className?: string }) {
  const { resolvedTheme, setTheme } = useTheme();
  const dark = resolvedTheme !== "light";
  return (
    <div className={`theme-switch ${className}`} role="group" aria-label="Color theme">
      <button
        type="button"
        onClick={() => setTheme("dark")}
        className={`theme-choice ${dark ? "active" : ""}`}
        aria-pressed={dark}
        title="Dark mode"
      >
        <Moon size={14} />
        <span>Dark</span>
      </button>
      <button
        type="button"
        onClick={() => setTheme("light")}
        className={`theme-choice ${!dark ? "active" : ""}`}
        aria-pressed={!dark}
        title="Light mode"
      >
        <Sun size={14} />
        <span>Light</span>
      </button>
    </div>
  );
}

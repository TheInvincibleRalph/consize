import type { Metadata } from "next";
import { Geist } from "next/font/google";
import "./globals.css";
import { ThemeProvider } from "@/components/ThemeProvider";
import { AuthProvider } from "@/components/auth";
import { Shell } from "@/components/Shell";

/* Bundled at build time by next/font — no runtime external fetch.
   Geist is the product grotesque (the Zorveus/Vercel standard, ADR-041);
   if the font fetch fails at build time, swap back to Inter — the two
   faces are metrics-compatible by design. */
const geist = Geist({
  subsets: ["latin"],
  variable: "--font-geist",
  display: "swap",
});

export const metadata: Metadata = {
  title: "conSize — infrastructure rightsizing",
  description:
    "Analyze, guardedly apply, verify, and audit infrastructure rightsizing recommendations.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    /* suppressHydrationWarning: next-themes stamps data-theme on <html>
       before hydration (ADR-040); the attribute must not be diffed. */
    <html lang="en" className={`${geist.variable} h-full antialiased`} suppressHydrationWarning>
      <body className="min-h-full">
        <ThemeProvider>
          <AuthProvider>
            <Shell>{children}</Shell>
          </AuthProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}

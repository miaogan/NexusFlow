import type { ButtonHTMLAttributes, ReactNode } from "react";

type Variant = "primary" | "ghost" | "danger" | "subtle";

const variantClasses: Record<Variant, string> = {
  primary:
    "bg-accent text-ink-950 hover:bg-accent/90 disabled:opacity-50 font-medium",
  ghost:
    "border border-white/10 text-slate-200 hover:border-accent/40 hover:text-accent disabled:opacity-50",
  danger:
    "border border-rose-500/40 text-rose-300 hover:bg-rose-500/10 disabled:opacity-50",
  subtle:
    "bg-white/5 text-slate-300 hover:bg-white/10 disabled:opacity-50",
};

export function Button({
  variant = "primary",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant }) {
  return (
    <button
      className={`inline-flex items-center justify-center gap-2 rounded-lg px-3.5 py-2 text-xs transition focus:outline-none focus:ring-2 focus:ring-accent/40 ${variantClasses[variant]} ${className}`}
      {...props}
    />
  );
}

export function Card({
  title,
  action,
  children,
  className = "",
}: {
  title?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={`panel ${className}`}>
      {(title || action) && (
        <header className="flex items-center justify-between border-b border-white/5 px-5 py-3">
          {title && <h3 className="panel-title">{title}</h3>}
          {action}
        </header>
      )}
      <div className="p-5">{children}</div>
    </section>
  );
}

export function Spinner({ className = "" }: { className?: string }) {
  return (
    <span
      className={`inline-block h-4 w-4 animate-spin rounded-full border-2 border-white/20 border-t-accent ${className}`}
    />
  );
}

export function EmptyState({ icon, title, hint }: { icon?: ReactNode; title: string; hint?: string }) {
  return (
    <div className="flex flex-col items-center justify-center py-16 text-center">
      {icon && <div className="mb-3 text-3xl opacity-40">{icon}</div>}
      <p className="text-sm font-medium text-slate-300">{title}</p>
      {hint && <p className="mt-1 max-w-sm text-xs text-slate-500">{hint}</p>}
    </div>
  );
}

const stateColors: Record<string, string> = {
  PENDING: "bg-slate-500/15 text-slate-300",
  FETCHED: "bg-sky-500/15 text-sky-300",
  PARSING: "bg-amber-500/15 text-amber-300",
  PARSED: "bg-amber-500/15 text-amber-300",
  CHUNKED: "bg-violet-500/15 text-violet-300",
  EMBEDDED: "bg-violet-500/15 text-violet-300",
  INDEXED: "bg-emerald-500/15 text-emerald-300",
  REINDEXING: "bg-sky-500/15 text-sky-300",
  COMPLETED: "bg-emerald-500/15 text-emerald-300",
  FAILED: "bg-rose-500/15 text-rose-300",
};

export function StatePill({ state }: { state: string }) {
  const cls = stateColors[state] ?? "bg-white/5 text-slate-300";
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[10px] font-medium ${cls}`}>
      <span className="h-1.5 w-1.5 rounded-full bg-current" />
      {state}
    </span>
  );
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-lg font-semibold text-slate-100">{title}</h1>
        {description && <p className="mt-1 text-xs text-slate-500">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  );
}

export function ProgressBar({ value }: { value: number }) {
  const pct = Math.max(0, Math.min(100, value));
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-white/5">
      <div
        className="h-full rounded-full bg-gradient-to-r from-accent/60 to-accent transition-all"
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

export function Tag({ children }: { children: ReactNode }) {
  return (
    <span className="rounded-md border border-white/10 bg-white/5 px-1.5 py-0.5 text-[10px] text-slate-400">
      {children}
    </span>
  );
}

export function IconTrash({ className = "" }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      width="14"
      height="14"
    >
      <path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6" />
    </svg>
  );
}

export function IconRefresh({ className = "" }: { className?: string }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      width="14"
      height="14"
    >
      <path d="M21 12a9 9 0 1 1-2.64-6.36M21 3v6h-6" />
    </svg>
  );
}

export function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

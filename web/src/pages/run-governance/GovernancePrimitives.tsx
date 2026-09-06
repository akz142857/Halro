import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { UsageAttempt } from "../../types";
import { money } from "../../format";
import { shortID } from "./governance-state";

export function GovernanceBadge({ tone, children }: { tone: "good" | "warning" | "danger" | "neutral"; children: React.ReactNode }) {
  return <span className={`governance-badge ${tone}`}><span aria-hidden="true" />{children}</span>;
}

export function CopyableID({ value, label }: { value: string; label: string }) {
  const { t } = useTranslation();
  const [status, setStatus] = useState<"idle" | "copied" | "failed">("idle");
  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setStatus("copied");
    } catch {
      setStatus("failed");
    }
    window.setTimeout(() => setStatus("idle"), 1800);
  }
  return <span className="governance-id"><code title={value}>{shortID(value)}</code><button type="button" className="icon-button governance-copy" aria-label={t("runGovernance.copyID", { label })} onClick={copy}>{status === "copied" ? "✓" : status === "failed" ? "!" : "⧉"}</button><span className="sr-only" aria-live="polite">{status === "copied" ? t("runGovernance.idCopied", { label }) : status === "failed" ? t("runGovernance.idCopyFailed", { label }) : ""}</span></span>;
}

export function WorkUnitStatusBadge({ status }: { status: "open" | "closed" }) {
  const { t } = useTranslation();
  return <GovernanceBadge tone={status === "open" ? "good" : "neutral"}>{t(`runGovernance.${status}`)}</GovernanceBadge>;
}

export function RunStatusBadge({ status }: { status: "active" | "closed" | "expired" }) {
  const { t } = useTranslation();
  return <GovernanceBadge tone={status === "active" ? "good" : status === "expired" ? "warning" : "neutral"}>{t(`runGovernance.${status}`)}</GovernanceBadge>;
}

export function BudgetStatusBadge({ status }: { status: "available" | "fully_reserved" | "depleted" }) {
  const { t } = useTranslation();
  return <GovernanceBadge tone={status === "available" ? "good" : status === "depleted" ? "danger" : "warning"}>{t(`runGovernance.${status}`)}</GovernanceBadge>;
}

export function CostEvidence({ attempt }: { attempt: UsageAttempt }) {
  const { t } = useTranslation();
  if (attempt.cost_value_status === "unknown" || attempt.cost_micros_usd == null) {
    return <span className="cost-evidence"><strong>—</strong><GovernanceBadge tone="warning">{t("runGovernance.costUnknown")}</GovernanceBadge></span>;
  }
  if (attempt.lease_mode === "free") {
    return <span className="cost-evidence"><strong>{money(attempt.cost_micros_usd)}</strong><GovernanceBadge tone="good">{t("runGovernance.freePriceVersion")}</GovernanceBadge></span>;
  }
  return <span className="cost-evidence"><strong>{money(attempt.cost_micros_usd)}</strong><GovernanceBadge tone={attempt.cost_estimated ? "warning" : "neutral"}>{t(attempt.cost_estimated ? "runGovernance.costEstimated" : "runGovernance.costKnown")}</GovernanceBadge></span>;
}

export function RunBudgetBar({ committed, reserved, budget }: { committed: number; reserved: number; budget: number }) {
  const { t } = useTranslation();
  const used = committed + reserved;
  const percent = budget > 0 ? Math.min(100, Math.max(0, used / budget * 100)) : 0;
  return <div className="run-budget"><div className="run-budget-label"><span>{t("runGovernance.budgetUsage")}</span><strong>{money(used)} / {money(budget)}</strong></div><div className="run-budget-track" role="progressbar" aria-label={t("runGovernance.budgetUsage")} aria-valuemin={0} aria-valuemax={budget} aria-valuenow={Math.min(used, budget)}><span style={{ width: `${percent}%` }} /></div><small>{t("runGovernance.budgetBreakdown", { committed: money(committed), reserved: money(reserved) })}</small></div>;
}

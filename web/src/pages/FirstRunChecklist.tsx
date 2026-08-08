import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState } from "../components";
import { Link } from "../navigation";
import { useIsReadOnly } from "../session";
import type { OnboardingGoal } from "../types";

export function FirstRunChecklist() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const readiness = useQuery({
    queryKey: ["onboarding-readiness"],
    queryFn: api.onboardingReadiness,
    refetchInterval: 15_000,
  });

  if (readiness.isPending) {
    return (
      <section className="panel first-run-panel first-run-loading" aria-labelledby="first-run-loading-title" aria-busy="true">
        <div>
          <p className="eyebrow">{t("dashboard.firstRun.eyebrow")}</p>
          <h2 id="first-run-loading-title">{t("dashboard.firstRun.loading")}</h2>
        </div>
        <span className="first-run-loading-line" aria-hidden="true" />
      </section>
    );
  }

  if (readiness.isError) {
    return (
      <section className="panel first-run-panel first-run-unavailable" aria-labelledby="first-run-unavailable-title">
        <header className="panel-header">
          <div>
            <p className="eyebrow">{t("dashboard.firstRun.eyebrow")}</p>
            <h2 id="first-run-unavailable-title">{t("dashboard.firstRun.unavailable")}</h2>
          </div>
        </header>
        <ErrorState error={readiness.error} className="first-run-error" />
        <div className="first-run-retry">
          <button className="button primary" onClick={() => void readiness.refetch()}>{t("common.retry")}</button>
        </div>
      </section>
    );
  }

  const data = readiness.data;
  if (data.state === "first_value_reached") return null;
  const current = data.goals.find((goal) => goal.state === "current" || goal.state === "error") ?? data.goals[0];
  const progressText = t("dashboard.firstRun.progress", { done: data.completed_goals, total: data.total_goals });

  return (
    <section className="panel first-run-panel" aria-labelledby="first-run-title">
      <header className="first-run-header">
        <div>
          <p className="eyebrow">{t("dashboard.firstRun.eyebrow")}</p>
          <h2 id="first-run-title">{t("dashboard.firstRun.title")}</h2>
          <p className="first-run-description">{t("dashboard.firstRun.description")}</p>
        </div>
        <div className="first-run-progress-shell">
          <strong>{progressText}</strong>
          <div
            className="first-run-progress-track"
            role="progressbar"
            aria-label={t("dashboard.firstRun.progressLabel")}
            aria-valuemin={0}
            aria-valuemax={data.total_goals}
            aria-valuenow={data.completed_goals}
            aria-valuetext={progressText}
          >
            <span style={{ width: `${data.total_goals ? data.completed_goals / data.total_goals * 100 : 0}%` }} />
          </div>
        </div>
      </header>

      <ol className="first-run-goals">
        {data.goals.map((goal, index) => <GoalRow goal={goal} index={index} key={goal.key} />)}
      </ol>

      {data.last_verification && (
        <div className="first-run-verification-error" role="alert">
          <div>
            <strong>{t("dashboard.firstRun.failure.title")}</strong>
            <span>{t("dashboard.firstRun.failure.description")}</span>
          </div>
          <dl>
            <div><dt>{t("dashboard.firstRun.failure.requestID")}</dt><dd><code>{data.last_verification.request_id}</code></dd></div>
            {data.last_verification.http_status ? <div><dt>HTTP</dt><dd>{data.last_verification.http_status}</dd></div> : null}
            {data.last_verification.error_class ? <div><dt>{t("dashboard.firstRun.failure.error")}</dt><dd><code>{data.last_verification.error_class}</code></dd></div> : null}
          </dl>
          <Link className="button ghost" href={`/admin/usage?request_id=${encodeURIComponent(data.last_verification.request_id)}`}>{t("dashboard.firstRun.failure.details")}</Link>
        </div>
      )}

      <footer className="first-run-action">
        <div>
          <span>{t("dashboard.firstRun.next")}</span>
          <strong>{t(`dashboard.firstRun.goals.${current.key}.title`)}</strong>
          <small>{readOnly ? t("dashboard.firstRun.readOnly") : t(`dashboard.firstRun.details.${current.detail_code}`)}</small>
        </div>
        {readOnly
          ? <button className="button primary first-run-action-button" disabled>{t("dashboard.firstRun.adminRequired")}</button>
          : (
            <Link className="button primary first-run-action-button" href={current.action_href}>
              <span>{t(`dashboard.firstRun.goals.${current.key}.action`)}</span>
              <span className="first-run-action-arrow" aria-hidden="true">→</span>
            </Link>
          )}
      </footer>
    </section>
  );
}

function GoalRow({ goal, index }: { goal: OnboardingGoal; index: number }) {
  const { t } = useTranslation();
  return (
    <li className={`first-run-goal ${goal.state}`} aria-current={goal.state === "current" || goal.state === "error" ? "step" : undefined}>
      <span className="first-run-goal-marker" aria-hidden="true">{goal.state === "complete" ? "✓" : index + 1}</span>
      <span className="first-run-goal-copy">
        <strong>{t(`dashboard.firstRun.goals.${goal.key}.title`)}</strong>
        <small>{t(`dashboard.firstRun.details.${goal.detail_code}`)}</small>
      </span>
      <span className={`first-run-goal-state ${goal.state}`}>{t(`dashboard.firstRun.states.${goal.state}`)}</span>
    </li>
  );
}

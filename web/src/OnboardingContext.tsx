import { useTranslation } from "react-i18next";
import { Link } from "./navigation";

export function hasOnboardingCreateIntent() {
  const params = new URLSearchParams(window.location.search);
  return params.get("onboarding") === "first-request" && params.get("intent") === "create";
}

export function OnboardingContextBanner() {
  const { t } = useTranslation();
  if (new URLSearchParams(window.location.search).get("onboarding") !== "first-request") return null;
  return (
    <div className="onboarding-context" role="status">
      <div>
        <strong>{t("dashboard.firstRun.context.title")}</strong>
        <span>{t("dashboard.firstRun.context.description")}</span>
      </div>
      <Link className="onboarding-context-back" href="/admin">
        <span className="onboarding-context-back-arrow" aria-hidden="true">←</span>
        <span>{t("dashboard.firstRun.context.back")}</span>
      </Link>
    </div>
  );
}

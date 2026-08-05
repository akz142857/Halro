import { useQueries, useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { StatusDot } from "../components";
import { Link } from "../navigation";

// The six-step configuration chain, in the order one step unlocks the next: a provider
// needs a credential, a deployment needs a provider, and so on down to the key that a
// real request finally presents. Rendered only on an instance that has never served
// traffic, so an established gateway never pays for these queries.
export function FirstRunChecklist() {
  const { t } = useTranslation();
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const routes = useQuery({ queryKey: ["routes"], queryFn: api.routes });
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const projectItems = projects.data?.items ?? [];
  const keyQueries = useQueries({
    queries: projectItems.map((project) => ({
      queryKey: ["project-keys", project.id],
      queryFn: () => api.keys(project.id),
    })),
  });
  const lists = [credentials, providers, deployments, routes, projects];
  // Silence rather than a half-read checklist: the dashboard reports its own failures,
  // and a step that reads "none yet" only because a request failed would send the
  // operator to create something they already have.
  if (lists.some((list) => list.isPending || list.isError)) return null;
  if (keyQueries.some((query) => query.isPending || query.isError)) return null;
  const steps = [
    { key: "credential", count: credentials.data?.items.length ?? 0, href: "/admin/providers?view=credentials" },
    { key: "provider", count: providers.data?.items.length ?? 0, href: "/admin/providers" },
    { key: "deployment", count: deployments.data?.items.length ?? 0, href: "/admin/deployments" },
    { key: "route", count: routes.data?.items.length ?? 0, href: "/admin/routes" },
    { key: "project", count: projectItems.length, href: "/admin/projects" },
    { key: "key", count: keyQueries.reduce((total, query) => total + (query.data?.items.length ?? 0), 0), href: "/admin/projects" },
  ];
  const done = steps.filter((step) => step.count > 0).length;
  return (
    <section className="panel first-run-panel" aria-labelledby="first-run-title">
      <header className="panel-header">
        <div>
          <p className="eyebrow">{t("dashboard.firstRun.eyebrow")}</p>
          <h2 id="first-run-title">{t("dashboard.firstRun.title")}</h2>
        </div>
        <strong className="first-run-progress">{t("dashboard.firstRun.progress", { done, total: steps.length })}</strong>
      </header>
      <p className="first-run-description">{t("dashboard.firstRun.description")}</p>
      <ol className="first-run-steps">
        {steps.map((step, index) => (
          <li className="first-run-step" key={step.key}>
            <span className="first-run-step-copy">
              <strong>
                <StatusDot ok={step.count > 0} label={t(step.count > 0 ? "dashboard.firstRun.doneLabel" : "dashboard.firstRun.pendingLabel")} />
                {index + 1}. {t(`dashboard.firstRun.${step.key}.title`)}
              </strong>
              <small>{step.count > 0 ? t("dashboard.firstRun.created", { count: step.count }) : t("dashboard.firstRun.none")}</small>
            </span>
            <Link className="button ghost" href={step.href}>{t(`dashboard.firstRun.${step.key}.action`)}</Link>
          </li>
        ))}
      </ol>
      <div className="first-run-verify">
        <span className="first-run-step-copy">
          <strong>{t("dashboard.firstRun.verify.title")}</strong>
          <small>{done === steps.length ? t("dashboard.firstRun.verify.ready") : t("dashboard.firstRun.verify.blocked")}</small>
        </span>
        <Link className="button primary" href="/admin/developer">{t("dashboard.firstRun.verify.action")}</Link>
      </div>
    </section>
  );
}

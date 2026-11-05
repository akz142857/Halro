import { useId } from "react";
import { useTranslation } from "react-i18next";
import { DisclosureSection } from "../components";
import type { GatewayScope } from "../types";

const modelScopes = [
  { value: "inference", copy: "inference" },
  { value: "discovery", copy: "discovery" },
] as const;

const governanceScopes = [
  { value: "work_unit:create", copy: "workUnitCreate" },
  { value: "run:create", copy: "runCreate" },
  { value: "run:attach", copy: "runAttach" },
  { value: "governance:read", copy: "governanceRead" },
  { value: "outcome:write", copy: "outcomeWrite" },
] as const;

export function GatewayKeyScopes({ value, onChange, disabled = false }: {
  value: GatewayScope[];
  onChange: (scope: GatewayScope, checked: boolean) => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  const id = useId();
  const selectedGovernance = governanceScopes.filter((scope) => value.includes(scope.value)).length;
  const option = (scope: (typeof modelScopes)[number] | (typeof governanceScopes)[number]) => {
    const titleID = `${id}-${scope.copy}-title`;
    const descriptionID = `${id}-${scope.copy}-description`;
    return (
      <label className="gateway-key-scope-option" key={scope.value}>
        <input type="checkbox" checked={value.includes(scope.value)}
          aria-labelledby={titleID} aria-describedby={descriptionID}
          onChange={(event) => onChange(scope.value, event.target.checked)} />
        <span className="gateway-key-scope-content">
          <span className="gateway-key-scope-heading">
            <strong id={titleID}>{t(`projects.keyScopeOptions.${scope.copy}.title`)}</strong>
            <code aria-hidden="true">{scope.value}</code>
          </span>
          <span className="gateway-key-scope-description" id={descriptionID}>{t(`projects.keyScopeOptions.${scope.copy}.description`)}</span>
        </span>
      </label>
    );
  };
  return (
    <fieldset className="gateway-key-scope-checks" disabled={disabled} aria-describedby={`${id}-hint`}>
      <legend>{t("projects.keyScopes")}</legend>
      <p className="gateway-key-scope-hint" id={`${id}-hint`}>{t("projects.keyScopesHint")}</p>
      <div className="gateway-key-scope-options" role="group" aria-label={t("projects.keyScopeModelAccess")}>
        {modelScopes.map(option)}
      </div>
      <DisclosureSection className="gateway-key-governance-scopes" title={t("projects.keyScopeGovernance")}
        summary={t(selectedGovernance ? "projects.keyScopeSelected" : "projects.keyScopeNone", { count: selectedGovernance })}>
        <p className="gateway-key-scope-hint">{t("projects.keyScopeGovernanceHint")}</p>
        <div className="gateway-key-scope-options" role="group" aria-label={t("projects.keyScopeGovernance")}>
          {governanceScopes.map(option)}
        </div>
      </DisclosureSection>
    </fieldset>
  );
}

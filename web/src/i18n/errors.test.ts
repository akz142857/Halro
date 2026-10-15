import { afterEach, describe, expect, it } from "vitest";
import { ApiError } from "../api";
import i18n, { applyLocale } from ".";
import { errorDetail, localizedError } from "./errors";

// The Admin API answers a refusal with a code and its own English sentence. The
// code is the only part the console can translate, so these two behaviours are
// what keep a Chinese operator from reading an English sentence: the code
// selects the copy, and the English original is then not repeated underneath a
// sentence that already said the same thing.
function refusal(code: string, message: string, status = 400) {
  return new ApiError(status, message, code);
}

describe("server refusals", () => {
  // The shared setup leaves both bundles resident and the language on zh-CN.
  afterEach(async () => {
    await applyLocale("zh-CN");
  });

  it("states a coded refusal in the reader's language, in both languages", async () => {
    const error = refusal("route_disabled", "route is disabled");

    expect(localizedError(i18n.t, error)).toBe("模型路由已禁用，请先启用它。");

    await applyLocale("en-US");
    expect(localizedError(i18n.t, error)).toBe("The route is disabled. Enable it first.");
  });

  it("translates every Run Governance consistency refusal", async () => {
    const cases = [
      ["governance_unavailable", "业务结果治理暂不可用。当前页面不会把缺失数据解释为零，请先检查系统状态。", "Business outcome governance is unavailable. This view will not interpret missing data as zero; check system status first."],
      ["run_governance_unavailable", "运行治理数据暂不可用。当前页面不会把不完整结果显示为完整，请先检查系统状态。", "Run Governance data is unavailable. This view will not show a partial result as complete; check system status first."],
      ["governance_summary_unavailable", "无法从一致的费用与结果快照生成治理汇总。请检查系统状态后重试。", "The governance summary could not be produced from a consistent accounting and outcome snapshot. Try again after checking system status."],
      ["governance_export_inconsistent", "治理导出无法取得一致的费用与结果快照，未接受不完整导出。", "The governance export could not capture a consistent accounting and outcome snapshot. No partial export was accepted."],
      ["governance_summary_overflow", "治理汇总超出支持的数值范围，请缩小统计群组后重试。", "The governance summary exceeds the supported numeric range. Narrow the cohort before trying again."],
    ] as const;

    for (const [code, zh] of cases) expect(localizedError(i18n.t, refusal(code, "server fallback", 503))).toBe(zh);
    await applyLocale("en-US");
    for (const [code, , en] of cases) expect(localizedError(i18n.t, refusal(code, "server fallback", 503))).toBe(en);
  });

  it("does not repeat the server's English sentence under the translated one", () => {
    expect(errorDetail(refusal("provider_disabled", "provider is disabled"))).toBe("");
    expect(errorDetail(refusal("invalid_request", "invalid request"))).toBe("");
  });

  it("still shows the server's reason when it has no code to translate", () => {
    // Domain validation prose has no bounded vocabulary, so it travels as the
    // server wrote it. Dropping it would leave the operator with a generic
    // headline and no field to fix.
    const error = new ApiError(400, "daily budget must not be negative");
    expect(localizedError(i18n.t, error)).toBe(i18n.t("errors.badRequest"));
    expect(errorDetail(error)).toBe("daily budget must not be negative");
  });

  it("reuses the console's own sentence rather than keeping a second copy of it", () => {
    // The delete button already explains both of these in a tooltip. The server
    // enforcing the same rule must not introduce a second wording of it.
    expect(localizedError(i18n.t, refusal("last_administrator", "cannot remove the last administrator account")))
      .toBe(i18n.t("adminUsers.cannotDeleteLastAdministrator"));
    expect(localizedError(i18n.t, refusal("cannot_delete_self", "use session/logout to end your own access")))
      .toBe(i18n.t("adminUsers.cannotDeleteSelf"));
  });
});

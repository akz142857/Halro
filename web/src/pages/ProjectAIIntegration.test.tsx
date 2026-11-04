import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import i18n from "../i18n";
import type { Project } from "../types";
import { ProjectAIIntegration } from "./ProjectAIIntegration";

const project: Project = {
  id: "prj_checkout", name: "结算项目", enabled: true, allowed_models: ["checkout-chat", "checkout-vector"],
  rpm: 60, tpm: 100000, max_concurrency: 8, daily_budget_micros_usd: 50000000,
  max_input_tokens: 0, max_output_tokens: 0, max_request_bytes: 0, max_stream_duration: 0,
  deferred_responses: false, max_deferred_queue: 0, allowed_cidrs: [], redaction_policy_id: "",
  token_guard_policy_id: "", revision: 3, created_at: "", updated_at: "",
};
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");
let copyText = vi.fn<(value: string) => Promise<void>>();

function renderIntegration(value = project) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["session"], { username: "viewer", role: "read_only" });
  const tree = (next: Project) => <QueryClientProvider client={client}><ProjectAIIntegration project={next} onClose={() => {}} /></QueryClientProvider>;
  const view = render(tree(value));
  return { ...view, updateProject: (next: Project) => view.rerender(tree(next)) };
}

function preview() {
  fireEvent.click(screen.getByText("预览接入说明", { selector: ".disclosure-title" }));
  return screen.getByRole("region", { name: "预览接入说明" });
}

function stubDownload() {
  const create = vi.fn((_blob: Blob | MediaSource) => "blob:project-skill");
  const revoke = vi.fn();
  const OriginalURL = URL;
  vi.stubGlobal("URL", class extends OriginalURL {
    static createObjectURL = create;
    static revokeObjectURL = revoke;
  });
  const downloaded: Array<{ href: string; filename: string }> = [];
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    downloaded.push({ href: this.href, filename: this.download });
  });
  return { create, revoke, click, downloaded };
}

function readBlob(blob: Blob) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error);
    reader.readAsText(blob, "utf-8");
  });
}

describe("project AI integration", () => {
  beforeEach(() => {
    copyText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: copyText } });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "https://gateway.example/v1", enabled: false });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    if (originalClipboard) Object.defineProperty(navigator, "clipboard", originalClipboard);
    else Reflect.deleteProperty(navigator, "clipboard");
  });

  it("exports project-scoped instructions for a read-only user even when the workbench is disabled", async () => {
    const execute = vi.spyOn(api, "developerExecute");
    const mint = vi.spyOn(api, "createKey");
    renderIntegration();
    const copy = await screen.findByRole("button", { name: "复制接入说明" });
    await waitFor(() => expect(copy).toBeEnabled());
    const options = Array.from(screen.getByRole("combobox", { name: "模型" }).querySelectorAll("option"), (option) => option.value);
    expect(options).toEqual(project.allowed_models);
    expect(screen.getByRole("combobox", { name: "API 接口" })).toHaveValue("responses");
    expect(screen.getByRole("region", { name: "预览接入说明" })).not.toBeVisible();
    fireEvent.click(copy);
    await screen.findByText("已复制接入说明");
    const instructions = preview().textContent!;
    expect(copyText).toHaveBeenCalledWith(instructions);
    expect(instructions).toContain(project.id);
    expect(instructions).toContain('"model": "checkout-chat"');
    expect(instructions).toContain("https://gateway.example/v1/responses");
    expect(instructions).not.toContain("/v1/v1/");
    expect(instructions).toContain("HALRO_API_KEY");
    expect(execute).not.toHaveBeenCalled();
    expect(mint).not.toHaveBeenCalled();
  });

  it("downloads a UTF-8 SKILL.md with the same instructions and revokes the object URL", async () => {
    const download = stubDownload();
    renderIntegration();
    await waitFor(() => expect(screen.getByRole("button", { name: "下载 SKILL.md" })).toBeEnabled());
    fireEvent.change(screen.getByRole("combobox", { name: "模型" }), { target: { value: "checkout-vector" } });
    fireEvent.change(screen.getByRole("combobox", { name: "API 接口" }), { target: { value: "embeddings" } });
    const instructions = preview().textContent!;
    fireEvent.click(screen.getByRole("button", { name: "下载 SKILL.md" }));
    expect(download.downloaded).toEqual([{ href: "blob:project-skill", filename: "SKILL.md" }]);
    await waitFor(() => expect(download.revoke).toHaveBeenCalledWith("blob:project-skill"));
    const blob = download.create.mock.calls[0][0] as Blob;
    expect(blob.type).toBe("text/markdown;charset=utf-8");
    const skill = await readBlob(blob);
    expect(skill).toMatch(/^---\nname:/);
    expect(skill).toContain(project.name);
    expect(skill.endsWith(instructions)).toBe(true);
    expect(skill).toContain("https://gateway.example/v1/embeddings");
    expect(skill).toContain('"model": "checkout-vector"');
    expect(screen.getByText("已开始下载")).toBeVisible();
    expect(screen.queryByText("已复制接入说明")).not.toBeInTheDocument();
    expect(copyText).not.toHaveBeenCalled();
  });

  it("keeps copy failures independent from download feedback", async () => {
    copyText.mockRejectedValue(new Error("clipboard denied"));
    stubDownload();
    renderIntegration();
    await waitFor(() => expect(screen.getByRole("button", { name: "复制接入说明" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("复制失败");
    fireEvent.click(screen.getByRole("button", { name: "下载 SKILL.md" }));
    expect(screen.getByText("已开始下载")).toBeVisible();
    expect(screen.getByRole("alert")).toHaveTextContent("复制失败");
    expect(screen.queryByText("已复制接入说明")).not.toBeInTheDocument();
  });

  it("reports download failure without claiming the file was downloaded", async () => {
    const download = stubDownload();
    download.click.mockImplementation(() => { throw new Error("download blocked"); });
    renderIntegration();
    await waitFor(() => expect(screen.getByRole("button", { name: "下载 SKILL.md" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "下载 SKILL.md" }));
    expect(screen.getByRole("alert")).toHaveTextContent("无法下载");
    expect(screen.queryByText("已开始下载")).not.toBeInTheDocument();
    expect(download.revoke).toHaveBeenCalledWith("blob:project-skill");
  });

  it("does not guess an Admin-origin URL or overwrite a manually entered URL when the config arrives", async () => {
    let resolve!: (value: { gateway_base_url: string; enabled?: boolean }) => void;
    vi.mocked(api.developerConfig).mockImplementation(() => new Promise((done) => { resolve = done; }));
    renderIntegration();
    expect(screen.getByLabelText(/^Gateway 地址/)).toHaveValue("");
    expect(screen.getByRole("button", { name: "复制接入说明" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/^Gateway 地址/), { target: { value: "https://manual.example/llm" } });
    expect(screen.getByRole("button", { name: "复制接入说明" })).toBeEnabled();
    await act(async () => resolve({ gateway_base_url: "http://127.0.0.1:28080", enabled: false }));
    expect(screen.getByLabelText(/^Gateway 地址/)).toHaveValue("https://manual.example/llm");
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    await waitFor(() => expect(copyText).toHaveBeenCalledWith(expect.stringContaining("https://manual.example/llm/v1/responses")));
  });

  it("lets a failed address read recover through manual input while rejecting unsafe URLs", async () => {
    vi.mocked(api.developerConfig).mockRejectedValue(new Error("config unavailable"));
    renderIntegration();
    expect(await screen.findByText("无法读取地址，请手动填写。")).toBeVisible();
    expect(screen.getByLabelText(/^Gateway 地址/)).toHaveValue("");
    for (const value of ["javascript:alert(1)", "https://user:secret@gateway.example", "https://gateway.example?key=secret", "https://gateway.example#fragment"]) {
      fireEvent.change(screen.getByLabelText(/^Gateway 地址/), { target: { value } });
      expect(screen.getByRole("button", { name: "复制接入说明" })).toBeDisabled();
      expect(screen.getByRole("button", { name: "下载 SKILL.md" })).toBeDisabled();
    }
    fireEvent.change(screen.getByLabelText(/^Gateway 地址/), { target: { value: "https://recovered.example" } });
    expect(screen.getByRole("button", { name: "复制接入说明" })).toBeEnabled();
    expect(copyText).not.toHaveBeenCalled();
  });

  it.each([
    { value: { ...project, enabled: false }, reason: "项目已停用，启用后可导出接入说明。" },
    { value: { ...project, allowed_models: [] }, reason: "项目尚未允许任何模型，请先配置模型权限。" },
  ])("keeps unavailable projects inspectable and prevents invented usable models ($reason)", async ({ value, reason }) => {
    renderIntegration(value);
    await waitFor(() => expect(screen.getByLabelText(/^Gateway 地址/)).toHaveValue("https://gateway.example/v1"));
    expect(screen.getByText(reason)).toBeVisible();
    expect(screen.getByRole("button", { name: "复制接入说明" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "下载 SKILL.md" })).toBeDisabled();
    expect(screen.queryByRole("region", { name: "预览接入说明" })).not.toBeInTheDocument();
    expect(copyText).not.toHaveBeenCalled();
  });

  it("clears stale feedback and follows the current project model permissions", async () => {
    const view = renderIntegration();
    await waitFor(() => expect(screen.getByRole("button", { name: "复制接入说明" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    await screen.findByText("已复制接入说明");
    view.updateProject({ ...project, revision: 4, allowed_models: ["replacement-chat"] });
    await waitFor(() => expect(screen.queryByText("已复制接入说明")).not.toBeInTheDocument());
    expect(screen.getByRole("combobox", { name: "模型" })).toHaveValue("replacement-chat");
    expect(screen.queryByRole("option", { name: "checkout-chat" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    await waitFor(() => expect(copyText).toHaveBeenLastCalledWith(expect.stringContaining('"model": "replacement-chat"')));
  });

  it("does not claim a previous copy succeeded for newly edited settings", async () => {
    let finish!: () => void;
    copyText.mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
    renderIntegration();
    await waitFor(() => expect(screen.getByRole("button", { name: "复制接入说明" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    expect(screen.getByText("复制中…")).toBeVisible();
    fireEvent.change(screen.getByLabelText(/^Gateway 地址/), { target: { value: "https://changed.example" } });
    await act(async () => finish());
    expect(screen.queryByText("已复制接入说明")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    await screen.findByText("已复制接入说明");
    expect(copyText).toHaveBeenLastCalledWith(expect.stringContaining("https://changed.example/v1/responses"));
  });

  it("preserves copy feedback with a warm config cache under StrictMode", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    client.setQueryData(["developer-config"], { gateway_base_url: "https://cached.example", enabled: false });
    render(<StrictMode><QueryClientProvider client={client}><ProjectAIIntegration project={project} onClose={() => {}} /></QueryClientProvider></StrictMode>);
    fireEvent.click(screen.getByRole("button", { name: "复制接入说明" }));
    expect(await screen.findByText("已复制接入说明")).toBeVisible();
    expect(copyText).toHaveBeenCalledWith(expect.stringContaining("https://cached.example/v1/responses"));
  });

  it("generates English instructions when the interface language is English", async () => {
    await i18n.changeLanguage("en-US");
    renderIntegration();
    await waitFor(() => expect(screen.getByRole("button", { name: "Copy instructions" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "Copy instructions" }));
    await waitFor(() => expect(copyText).toHaveBeenCalledWith(expect.stringContaining("# Integrate this Halro project")));
  });
});

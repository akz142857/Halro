import {
  createColumnHelper,
  flexRender,
  getCoreRowModel,
  useReactTable,
} from "@tanstack/react-table";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
} from "../components";
import type { Deployment, Provider, Route } from "../types";

const column = createColumnHelper<Route>();

export function RoutesPage() {
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Route>();
  const [testResult, setTestResult] = useState("");
  const routes = useQuery({ queryKey: ["routes"], queryFn: api.routes });
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: (route: Route) => api.deleteRoute(route.id, route.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["routes"] }),
  });
  const test = useMutation({
    mutationFn: (route: Route) => api.testRoute(route.id),
    onSuccess: (value) => setTestResult(`HEALTHY · ${value.latency_ms}ms`),
    onError: () => setTestResult("UNHEALTHY"),
  });
  const deploymentByID = useMemo(
    () => new Map(deployments.data?.items.map((item) => [item.id, item]) ?? []),
    [deployments.data],
  );
  const providerNames = useMemo(
    () => new Map(providers.data?.items.map((item) => [item.id, item.name]) ?? []),
    [providers.data],
  );
  const columns = useMemo(() => [
    column.accessor("public_model", {
      header: "PUBLIC MODEL",
      cell: ({ row, getValue }) => (
        <div className="model-cell">
          <StatusDot ok={row.original.enabled} />
          <strong>{getValue()}</strong>
          <code>{row.original.id}</code>
        </div>
      ),
    }),
    column.accessor("deployment_id", {
      header: "DEPLOYMENT",
      cell: ({ getValue }) => {
        const deployment = deploymentByID.get(getValue());
        return deployment?.name || getValue() || "Legacy route";
      },
    }),
    column.display({
      id: "provider",
      header: "PROVIDER",
      cell: ({ row }) => {
        const deployment = deploymentByID.get(row.original.deployment_id);
        const providerID = deployment?.provider_id || row.original.provider_id || "";
        return providerNames.get(providerID) || providerID;
      },
    }),
    column.display({
      id: "provider_model",
      header: "UPSTREAM MODEL",
      cell: ({ row }) => deploymentByID.get(row.original.deployment_id)?.provider_model || row.original.provider_model,
    }),
    column.accessor("strategy", {
      header: "STRATEGY",
      cell: ({ getValue }) => <span className="badge">{getValue() || "ordered"}</span>,
    }),
    column.accessor("priority", { header: "PRIORITY" }),
    column.display({
      id: "actions",
      header: "",
      cell: ({ row }) => (
        <div className="row-actions">
          <button className="button ghost" disabled={!row.original.enabled || test.isPending} onClick={() => test.mutate(row.original)}>测试</button>
          <button className="button ghost" onClick={() => setEditing(row.original)}>编辑</button>
          <ConfirmButton
            label="删除"
            confirmLabel={`删除 Route “${row.original.public_model}”？`}
            onConfirm={() => remove.mutate(row.original)}
            disabled={remove.isPending}
          />
        </div>
      ),
    }),
  ], [deploymentByID, providerNames, remove, test]);
  const table = useReactTable({
    data: routes.data?.items ?? [],
    columns,
    getCoreRowModel: getCoreRowModel(),
  });
  const pending = routes.isPending || deployments.isPending || providers.isPending;
  const error = routes.error || deployments.error || providers.error;
  return (
    <>
      <PageHeader
        eyebrow="MODEL FABRIC"
        title="Routes"
        description="公共模型别名只引用 Deployment；模型、价格、能力和并发策略由 Deployment 独立维护。"
        action={<button className="button primary" onClick={() => setCreating(true)}>＋ 新建 Route</button>}
      />
      {pending && <Loading />}
      {error && <ErrorState error={error} />}
      {testResult && <div className={`notice ${testResult.startsWith("HEALTHY") ? "success" : "warning"}`}><strong>{testResult}</strong></div>}
      {routes.data?.items.length === 0 && (
        <EmptyState title="还没有模型路由">创建 Route 后，OpenAI Compatible API 才能解析公共模型别名。</EmptyState>
      )}
      {!!routes.data?.items.length && (
        <div className="table-shell">
          <table>
            <caption className="visually-hidden">模型路由列表</caption>
            <thead>
              {table.getHeaderGroups().map((group) => (
                <tr key={group.id}>
                  {group.headers.map((header) => (
                    <th scope="col" key={header.id}>{flexRender(header.column.columnDef.header, header.getContext())}</th>
                  ))}
                </tr>
              ))}
            </thead>
            <tbody>
              {table.getRowModel().rows.map((row) => (
                <tr key={row.id}>
                  {row.getVisibleCells().map((cell) => (
                    <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {creating && (
        <RouteForm deployments={deployments.data?.items ?? []} onClose={() => setCreating(false)} />
      )}
      {editing && (
        <RouteForm current={editing} deployments={deployments.data?.items ?? []} onClose={() => setEditing(undefined)} />
      )}
    </>
  );
}

function RouteForm({ current, deployments, onClose }: { current?: Route; deployments: Deployment[]; onClose: () => void }) {
  const enabled = deployments.filter((item) => item.enabled || item.id === current?.deployment_id);
  const [publicModel, setPublicModel] = useState(current?.public_model ?? "chat");
  const [deploymentID, setDeploymentID] = useState(current?.deployment_id ?? enabled[0]?.id ?? "");
  const [priority, setPriority] = useState(current?.priority ?? 10);
  const [strategy, setStrategy] = useState<"ordered" | "round_robin">(current?.strategy || "ordered");
  const [routeEnabled, setRouteEnabled] = useState(current?.enabled ?? true);
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      public_model: publicModel,
      deployment_id: deploymentID,
      priority,
      strategy,
      enabled: routeEnabled,
      };
      return current
        ? api.updateRoute(current.id, value, current.revision)
        : api.createRoute(value);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["routes"] });
      onClose();
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (publicModel.trim() && deploymentID) mutation.mutate();
  };
  return (
    <Modal title={current ? "编辑模型 Route" : "创建模型 Route"} onClose={onClose}>
      {enabled.length === 0 ? (
        <div className="notice warning"><strong>需要可用 Deployment</strong><span>先在 Deployments 页面创建并启用一个模型部署。</span></div>
      ) : (
        <form className="form-grid" onSubmit={submit}>
          <Field label="公共模型别名"><input autoFocus required value={publicModel} onChange={(event) => setPublicModel(event.target.value)} /></Field>
          <Field label="Deployment">
            <select required value={deploymentID} onChange={(event) => setDeploymentID(event.target.value)}>
              {enabled.map((deployment) => (
                <option value={deployment.id} key={deployment.id}>{deployment.name} · {deployment.provider_model}</option>
              ))}
            </select>
          </Field>
          <Field label="路由策略">
            <select value={strategy} onChange={(event) => setStrategy(event.target.value as typeof strategy)}>
              <option value="ordered">Ordered fallback</option>
              <option value="round_robin">Round robin</option>
            </select>
          </Field>
          <Field label="优先级"><input type="number" value={priority} onChange={(event) => setPriority(Number(event.target.value))} /></Field>
          <label className="check-row"><input type="checkbox" checked={routeEnabled} onChange={(event) => setRouteEnabled(event.target.checked)} />启用 Route</label>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions">
            <button type="button" className="button ghost" onClick={onClose}>取消</button>
            <button className="button primary" disabled={mutation.isPending}>{current ? "保存并热加载" : "创建并热加载"}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}

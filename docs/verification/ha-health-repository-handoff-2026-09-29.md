# HA 健康系统仓库交付核对（2026-09-29）

本记录核对源码提交 `9e80f541cd8bb4b4db8ffe7cccf23ef4b8c3be9c`。
该提交包含独立健康服务、HA 指标与告警、机器状态和持久迁移证据采集、
客户端最终结果的**消费端契约**、只读页面、发布归档配置及 H10 合成浏览器
证据。后续如仅修改本文档，按仓库验证政策不重复运行已通过的代码门禁。

## 当前仓库门禁

| 范围 | 命令或检查 | 结果 |
| --- | --- | --- |
| Go 全仓 | `GOCACHE=/tmp/halro-go-cache go test -count=1 ./...` | 退出码 0；日志 SHA-256 `6799b5b04bc897b9dcb9f315190c3a7dc4ece516f50bca09c080afb8a41e1e67` |
| Go 静态检查与格式 | `go vet ./...`、`make fmt-check` | 均退出码 0 |
| 前端行为 | Node 22 下 `cd web && npm test` | 48 个文件、703 项测试通过；日志 SHA-256 `dc04dc628b083b27c057f53cd0817e602d99c2e59344f6286a886342adbc0722` |
| 前端生产构建 | Node 22 下 `make frontend-production-check`，再核对 `git diff --exit-code -- internal/webui/dist` | 构建、类型检查、产物检查和嵌入 bundle 漂移检查通过；日志 SHA-256 `ac091485e0e5bcc85b3c19766ee5697aa6de51bf96fb42f043d7f13d0c45bbaa` |
| 观测配置 | `deploy/observability/validate.sh` | Prometheus 配置、14 条记录规则、64 条告警规则及 Alertmanager 配置通过；日志 SHA-256 `1746a92e5c9c6b12fe54e50f800863243183103177220740dce5399df523e2dc` |
| 发布与页面小门禁 | `python3 -m unittest tools.release.test_release_workflow_contract`、`node --check cmd/halro-ha-health/ui/app.js` | 工作流契约运行 15 项，14 项通过、1 项原有跳过；脚本语法通过 |

本机发布模拟从干净 Git 归档生成 Darwin arm64 tar.gz，解包后的三个
二进制报告相同版本、提交与日期，健康服务部署指南逐字节一致，两次
归档 SHA-256 均为
`c72f725531c10e63e05c28b9a4ab85711f1b170b168c47cd97223399e1686220`。
解包后的独立服务又通过一次性证书完成 mTLS 页面 200、无证书握手拒绝及
证书指纹访问审计的本机烟测。它不是正式 GitHub 发布工作流或生产安装。

H10 修复后的源码在隔离 Chrome 中保存了[时钟偏差及慢响应的截图和
同会话请求时间线](evidence/ha-health-h10-local/manifest.json)。两次时钟
偏移各为一小时；迟到的 `/api/health` 响应耗时 31.015 秒，超过 30 秒后
五张卡、覆盖和节点均转未知或陈旧，响应返回后没有复活旧绿灯。测试只用
合成两成员 HTTP 替身，运行中的 kind 镜像来自更早的源码提交。

## 本次交付与正式验收的边界

仓库中的 P0 监控规则、数据契约、独立页面和相应验证已可复核；P1 的
机器状态、诊断事件、持久迁移采集和客户端最终结果**消费端**也已交付。
以下条件未由仓库测试或本机烟测证明，不能标记为通过：

1. 负责完整重试及响应消费的真实 SDK/入口代理责任方尚未确定，因此
   客户端最终逻辑操作结果没有生产来源，页面保持“未接入”。
2. 尚无独立不可变存储的归档回执、保留版本与读回；本机 kind PVC、
   文件哈希和本机副本核验不能替代外部交接。
3. 正式操作员身份/受控代理、独立通知联系点、跨故障域访问、完整 H01–H20
   故障矩阵、G0–G7、相邻版本及 RTO/RPO 尚未逐项签署。本地三节点 kind
   共用一台物理主机，不能证明生产故障域独立。
4. 用户决定本次本地交付**不等待 72 小时长跑**。已有 Job 与样本保留，
   本次不将其记为 PASS，也不将它作为仓库交付的阻塞项。

目标环境逐项条件及局部证据见[HA 健康系统本地验收台账](ha-health-local-acceptance-2026-09-28.md)
与[HA 测试指南 §4](ha-test-guide.zh-CN.md#4-ha-健康系统目标环境验收)。

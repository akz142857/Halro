# Halro 生产环境 Admin 初始化凭据交付方案

- 状态：Implemented（代码、清单、文档与自动化门禁完成；目标集群验收按环境执行）
- 日期：2026-09-17
- 目标版本：Unreleased
- 文档语言：中文
- 适用范围：Admin 首次初始化、CLI、Kubernetes 部署与运维文档

## 0. 决策摘要

Halro 不再把“读取进程启动日志”作为远程生产环境获取 Setup Token 的正式方案。

本方案提供两条生产路径：

1. **交互式初始化**：部署系统在 Halro 启动前生成 Setup Token，存入 Kubernetes Secret 或外部 Secret Manager，并以只读文件挂载给 Halro；负责首次初始化的人员通过组织已有的秘密审批与读取通道取得同一个 Token。
2. **自动化初始化（目标态推荐）**：部署流水线在主服务启动前运行一次性离线 Bootstrap Job，直接创建首个管理员；主容器随后使用 `halro serve` 启动，不产生 Setup Token。只有持久 Intent/Completion、PVC fencing 和故障注入验收全部完成后，才能标记为生产可用。

本地开发继续保留 `halro start` 在控制终端显示一次性 Token 的便利行为。生产部署示例不得再依赖该输出。

关键边界：

- Halro 不调用 Kubernetes API 创建或读取 Secret，不获得 Secret 管理权限；
- Setup Token 和管理员密码不得进入应用日志、结构化日志、Pod Event、命令行参数或普通配置值；
- 浏览器与公开 Admin API 不提供“读取 Setup Token”的接口；
- 对当前持久化实例，首个管理员提交完成后 Token 不再被接受；恢复到初始化前备份、替换数据目录或重建实例属于新的 bootstrap 边界；
- Kubernetes Secret 只是可选载体，外部 Secret Manager + CSI/Injector 是更高安全等级的部署方式。

---

## 1. 背景与当前问题

当前首次启动流程在尚无管理员时生成高熵 Setup Token。只要 Admin 使用非回环监听，或配置了 `admin.external_origin`，浏览器创建首个管理员时就必须提交该 Token。

当前 `halro start` 在监听器成功绑定后将 Token 直接写入进程 stderr：

```text
One-time setup token: setup_...
```

该实现适合本机终端，但不适合 Kubernetes 等远程生产环境：

1. 负责应用初始化的工程师可能没有 `pods/log` 权限；
2. 日志通常被采集到共享日志平台，读取范围可能比 Secret 权限更广；
3. 日志具有复制、索引、缓存和长期保留特性，不是秘密交付通道；
4. 当前镜像默认运行 `halro serve`，而 `serve` 不显示 Token；如果实例尚无管理员，操作者会得到一个要求 Token、却没有正式取回路径的页面；
5. 为解决初始化问题而授予 `kubectl logs` 或 `kubectl exec` 权限，会扩大生产访问面，并混淆观测权限和密钥权限。

根本问题不是“命令写在哪里”，而是秘密由工作负载内部生成后，只能反向穿过观测通道交给人。生产方案应由部署控制面提前交付秘密，或者完全自动完成初始化。

### 1.1 当前已有的安全基础

本方案复用而不改变以下约束：

- Setup Token 只在首个管理员不存在时有效；
- Token 使用常量时间比较；
- 首个管理员创建使用存储层原子约束，并发请求只能成功一次；
- 初始化成功后，进程内 Token 被清空；
- `halro admin bootstrap` 是离线操作，会获取数据目录独占锁；
- 首个管理员创建当前会尝试写入可信 Audit 链，但管理员记录和 Audit 追加尚非原子提交；本方案必须补齐持久 Audit Intent 后，才能把它作为生产保证；
- 本地 loopback 初始化仍可在同源限制下免 Token。

---

## 2. 行业参考

成熟开源系统普遍把初始化凭据放在部署 Secret 或 Bootstrap 配置中，而不是应用日志：

| 系统 | 生产初始化方式 | 生命周期 |
| --- | --- | --- |
| Argo CD | 自动生成初始管理员密码并保存为 `argocd-initial-admin-secret` | 首次改密后删除 Secret，并建议切换 SSO |
| GitLab Helm Chart | `shared-secrets` Job 生成或接受预创建的初始 root 密码 Secret | 首次登录后轮换；部署方可完全控制 Secret 来源 |
| Grafana | Helm Chart 使用 Secret；容器支持 `GF_SECURITY_ADMIN_PASSWORD__FILE` 从文件读取 | 首次创建管理员时消费 |
| Keycloak | 通过 bootstrap admin 配置创建临时管理员 | 仅在首次创建 master realm 时生效 |

参考资料：

- [Argo CD initial password](https://argo-cd.readthedocs.io/en/latest/user-guide/commands/argocd_admin_initial-password/)
- [Argo CD user management](https://argo-cd.readthedocs.io/en/stable/operator-manual/user-management/)
- [GitLab shared-secrets Job](https://docs.gitlab.com/charts/charts/shared-secrets/)
- [GitLab chart secrets](https://docs.gitlab.com/charts/installation/secrets/)
- [Grafana Helm deployment](https://grafana.com/docs/grafana/latest/setup-grafana/installation/helm/)
- [Grafana Docker Secrets](https://grafana.com/docs/grafana/latest/setup-grafana/configure-docker/)
- [Keycloak bootstrap admin configuration](https://www.keycloak.org/server/all-config)
- [Kubernetes Secrets security guidance](https://kubernetes.io/docs/concepts/security/secrets-good-practices/)

Halro 不需要复制任何一个产品的全部机制，但应采用相同原则：初始化凭据由秘密通道交付，权限可独立授权、审计和撤销。

---

## 3. 目标与非目标

### 3.1 目标

1. 远程生产部署不读取日志也能安全完成首次 Admin 初始化。
2. 支持 Kubernetes Secret、Vault Agent、Secrets Store CSI Driver 等文件挂载方式，不绑定某个云或密钥产品。
3. 支持无人值守的一次性 Bootstrap Job。
4. 保留本地开发的一命令体验。
5. 不给 Halro 工作负载增加 Kubernetes API 权限。
6. 初始化凭据的来源、使用和失效具备明确的操作与审计边界。
7. 对已初始化实例无迁移要求；删除 bootstrap Secret 后仍可正常重启。

### 3.2 非目标

第一版不包含：

- 在 Halro 内实现 Vault、AWS Secrets Manager、GCP Secret Manager 或 Azure Key Vault 客户端；
- 由 Halro 创建、更新或删除 Kubernetes Secret；
- 通过 Admin HTTP API 返回 Setup Token；
- OIDC/SAML 管理员组自动映射；这是后续可替代本地 bootstrap 的独立能力；
- 将 Kubernetes Secret 视为无需加密、RBAC 或审计的安全存储；
- 在线重置已有管理员密码；现有离线 break-glass 流程保持不变。

### 3.3 威胁模型与信任边界

本方案防护：

- 只能读取应用日志、但无 Secret 权限的人员；
- 普通研发账号和只读集群观察者；
- 网络窃听者，以及误配置的 Ingress、WAF、service mesh、APM 和错误采样；
- Setup Token 泄露后的重放；
- 初始化 Job 重试、Pod 重建和进程崩溃造成的半完成状态。

受信任主体是 Bootstrap 审批人、受控部署流水线和 Secret Manager。集群管理员、节点 root、能够任意修改目标工作负载或在同 namespace 创建/注入 Pod 的主体，通常可以间接取得挂载秘密；应用层不能对这些权限建立虚假隔离，平台必须用审计、准入策略和双人审批约束它们。

安全目标是：秘密不进入日志或持久业务数据、每实例唯一、限时有效、一次消费、首个管理员与 Audit Intent 原子提交、重试可判定、操作可归因，以及任何不确定状态 fail-closed。Token 文件故障时，可用性不优先于这些目标。

---

## 4. 产品模式

### 4.1 模式 A：本地交互式启动

适用：开发机、评估环境、操作者直接控制进程终端的单机部署。

```bash
halro start --config ./config.yaml
```

行为保持：

- 自动初始化空数据目录；
- 远程 Admin 配置需要 Token 时，在控制终端显示进程级一次性 Token；
- 重启且仍无管理员时生成新 Token；
- 文档明确该模式不是 Kubernetes 生产部署方式。

如果配置了 `admin.setup_token_file`，即使使用 `start` 也必须采用文件中的 Token，且不得再次打印 Token。

### 4.2 模式 B：远程交互式初始化

适用：必须由指定人员在浏览器中选择管理员用户名和密码，但该人员没有生产 Pod 日志权限。

配置新增：

```yaml
admin:
  setup_token_file: "/run/secrets/halro/setup-token"
  setup_token_ttl: "30m"
```

远程交互式路径仍然需要先初始化系统存储。对新 PVC，部署控制面先运行一次性 Init Job：

```bash
halro init --if-needed --config /etc/halro/config.yaml
```

Init Job 成功后，主 Deployment 才以 `halro serve` 启动。该 Job 只初始化系统存储，不创建管理员，也不读取 Setup Token。已有完整系统状态时成功空操作；残缺状态仍然 fail-closed。显式 CLI 的 `--if-needed` 必须是独立入口，不得放宽 `start` 对空 `key_slots` 实例禁止自动初始化的现有规则。

部署系统负责：

1. 使用 CSPRNG 生成 32 字节随机值，编码成规定的 `setup_` Token，并与绝对 UTC `expires_at` 一起写入凭据文件；每个实例和环境使用不同值；
2. 保存到受审计的 Secret Manager，或创建专用 Kubernetes Secret；
3. 只读挂载为文件；
4. 通过独立权限将 Token 交给获准执行初始化的人员；
5. 首个管理员创建成功后，按 §8.3 的顺序解除工作负载挂载依赖并撤销 bootstrap Secret。

Halro 负责：

1. 仅在数据库中不存在管理员且当前部署要求 Token 时读取文件；
2. 对文件信封、Token 和绝对过期时间执行固定格式与有界长度校验；Halro 不声称能从字符串内容判断实际熵；
3. 将 Token 转成固定长度摘要用于后续比较，尽量缩短明文驻留时间，不把 Token 写入数据库和日志；
4. 对浏览器提交值计算同类摘要后做常量时间比较；
5. 首个管理员与持久 Audit Intent 的事务提交后，立即移除运行时 Token 状态；
6. 已存在管理员时应用不再读取该文件；编排层能否在 Secret 缺失时重建 Pod 另由 `optional` volume 或解除挂载依赖保证，见 §8.3。

文件型 Token 在进程启动时读取一次，不热重载。修改或删除 Secret 不会撤销旧进程已加载的值；轮换必须阻断 Admin 入口或将 Deployment 缩容至 0，再启动读取新 Secret 的 Pod。文件包含生成时确定的绝对 `expires_at`，重启或重调度不得延长同一文件的有效期；`setup_token_ttl` 同时限制生成模式的有效期和文件在加载时允许的最大剩余有效期，默认 30 分钟、上限 24 小时。过期后 Setup API 保持 fail-closed，必须生成新文件、轮换 Secret 并重启。禁止使用 `subPath` 挂载 Token 文件。

### 4.3 模式 C：自动化离线初始化（目标态生产推荐）

适用：GitOps、CI/CD、受控生产发布和无人值守环境。

流程：

```text
创建 PVC / 配置 / bootstrap Secret
             ↓
运行一次性离线 Bootstrap Job
  - 初始化空数据目录
  - 创建首个管理员
             ↓
确认 Job 成功并保留审计结果
             ↓
删除或撤销集群内 bootstrap Secret
             ↓
部署主容器：halro serve
```

为避免 distroless 镜像依赖 shell 和管道，CLI 新增文件输入：

```bash
halro admin bootstrap \
  --config /etc/halro/config.yaml \
  --username admin \
  --password-file /run/secrets/halro/admin-password
```

现有 stdin 输入继续兼容，但官方容器示例使用 `--password-file`。

一次性 Job 必须在主 Deployment 启动前运行，因为 `halro init` 和 `halro admin bootstrap` 都是离线操作并要求数据目录独占。不得在正在运行的 Halro Pod 上通过 `kubectl exec` 执行。

为了支持 Job 安全重试，增加：

```bash
halro init --if-needed --config /etc/halro/config.yaml
halro admin bootstrap --if-needed --operation-id <stable-non-secret-id> ...
```

语义：

- 完全未初始化时执行初始化；
- 已存在完整系统状态时 `init --if-needed` 成功退出且不改写任何数据；
- 部分初始化、文件不匹配、数据损坏等状态仍然 fail-closed；
- 不得把 `--if-needed` 变成重置或覆盖现有管理员的入口。

`admin bootstrap --if-needed` 不能以“存在任意管理员”为成功条件。CLI 必须携带非秘密的稳定 `--operation-id`，并实现持久 Bootstrap Intent/Completion：

1. 首次执行时，operation ID、目标用户名、首个管理员记录和 `admin.bootstrap` Audit Intent 在同一个 BoltDB 事务中提交；完成记录不包含密码、Token、密码摘要或 Secret 标识；
2. Audit 日志交付失败不得丢失 Intent，启动恢复流程或同 operation ID 的重试负责重放；
3. 同 operation ID 且目标一致时先重放/确认持久 Intent 与 checkpoint，再返回明确的 `already_completed`，不覆盖账号或密码；
4. operation ID 不同、用户名不匹配、存在管理员却没有匹配的完成记录，或状态无法确定时必须失败；
5. 新建成功返回 `created`；任何 durable write 之间的崩溃都必须能恢复为确定状态，而不是被下一次执行静默掩盖。

带显式 `--operation-id` 的自动化路径成功时，stdout 使用固定单行格式 `Admin bootstrap result: <created|already_completed> (operation_id=<id>)`；两种成功结果退出码均为 0。冲突、歧义、输入或基础设施错误返回非 0，自动化应优先以退出码判断成功，再以固定结果字段区分新建与幂等重放，不解析其他人类可读错误文案。

Web Setup 使用服务端生成的 operation ID，并复用同一事务协议。管理员事务已经提交但 Session 创建失败时，初始化仍视为完成，Token 不得恢复有效；用户转到正常登录流程。

官方 Kubernetes 示例可使用同一个 Job Pod 的 init container 执行 `init --if-needed`，主 Job container 执行 `admin bootstrap --if-needed --operation-id ... --password-file ...`，两者共享 PVC。Job 成功后再创建 Halro Deployment。

---

## 5. 配置、CLI 与 HTTP 契约

### 5.1 `admin.setup_token_file` 与 `admin.setup_token_ttl`

新增可选字符串字段，默认空值：

```yaml
admin:
  setup_token_file: ""
  setup_token_ttl: "30m"
```

规则：

- 路径应为绝对路径；相对路径在配置校验阶段拒绝，避免工作目录变化读取错误文件；
- 配置解析和 `halro config check` 只验证字段形态，不读取文件；
- 只在“无管理员且 Setup Token 必需”时读取；
- 文件缺失、不可读、为空或格式不合法时，服务启动失败；
- 文件是两行 UTF-8/ASCII 信封：第一行为 Token，第二行为 `expires_at=<RFC3339Nano UTC>`；接受文件末尾单个 `LF` 或 `CRLF`，不自动去除其他空白或额外行；
- Token 固定为 `setup_` 前缀加 32 字节 CSPRNG 输出的无填充 base64url 编码，总长度 49 个 ASCII 字符；`generated` 和文件信封内的 Token 使用同一规范；
- Halro 只校验固定格式和长度；由合格 CSPRNG 生成、保持单实例唯一且不跨环境复用，是部署系统的责任；
- `setup_token_ttl` 默认 `30m`，必须大于 0，最大 `24h`；生成模式从启动时计时，文件模式要求 `expires_at` 不晚于加载时刻加该值；已过期文件可以被读取为失效状态但不会因重启续期；
- 错误信息可以包含配置字段名，但不得回显文件内容；
- 文件路径不是凭据，但结构化日志仍只记录 `setup_token_source=file`，不记录具体路径。

当该字段非空时，即使 Admin 绑定 loopback，也显式要求 Setup Token。这使安全要求可以由部署配置决定，而不是依赖网络地址推断。

为没有原生随机秘密生成能力的部署系统提供离线辅助命令：

```bash
halro admin setup-token generate --ttl 30m --output /secure/path/setup-token
```

该命令使用专用 32 字节 CSPRNG 生成器，以 `0600`、排他创建方式写入 Token 与绝对过期时间，目标已存在时拒绝覆盖；`--ttl` 默认 30 分钟、最大 24 小时。它不把 Token 写到 stdout/stderr。上传到 Secret Manager 后，临时文件按组织安全流程销毁。现有通用 `id.New("setup")` 只有 128 bit 随机输入，不作为新 Token 规范的实现。

### 5.2 运行时 Token 来源

内部将 Token 来源建模为显式枚举，而不是用空字符串推断：

```text
none       本地同源初始化，不需要 Token
generated  halro start 的交互式临时 Token
file       外部秘密文件
```

优先级：

1. 已存在管理员：`none`，不读取文件；
2. 配置 `setup_token_file`：`file`；
3. Token 非必需：`none`；
4. `halro start`：`generated`；
5. `halro serve`：拒绝启动，并提示配置 `admin.setup_token_file` 或先执行离线 `admin bootstrap`。

因此，`serve` 不会创建一个操作者无法取回的秘密；`start` 的终端输出语义也不再被生产入口隐式继承。文件模式的秘密不得通过现有 `SetupToken()` 等展示接口返回；生成模式只允许启动引导代码消费一次明文用于控制终端输出。

兼容性说明：HTTP Setup Status 与 Setup 请求体保持不变，已初始化实例不受影响。行为变化只影响“零管理员 + 需要 Token + `serve` + 未配置 Token 文件”的实例：升级后它们会在开始服务前失败。发布说明必须要求操作者先配置 `setup_token_file`，或先执行离线 Admin bootstrap。

### 5.3 Setup Status API

公开状态响应保持最小化：

```json
{
  "instance_initialized": true,
  "setup_required": true,
  "token_required": true
}
```

不得增加 Token、文件路径、Secret 名称或 Token 来源字段。前端只需要知道是否展示 Token 输入框。
`instance_initialized` 表示系统存储已经初始化，不表示首个 Admin 已创建；两者分别由 `instance_initialized` 和 `setup_required` 表达。Token 过期也不新增公开状态位，提交时继续返回统一无效凭据响应；服务不根据错误值在响应或日志中区分“已过期”和“错误 Token”，恢复步骤由运维文档提供。

### 5.4 CLI 密码文件

`halro admin bootstrap` 和 `halro admin reset-password` 同时支持：

- 未指定 `--password-file` 时从 stdin 读取，保持现有行为；
- 指定 `--password-file <absolute-path>` 时只读取该文件，完全不读取或探测 stdin，避免无人值守进程阻塞；
- 不提供 `--password <value>`，防止密码进入 shell history 和进程参数；
- 两种来源使用同一套 1024 字节上限、密码策略和末尾单个 `LF`/`CRLF` 处理，不执行 `TrimSpace`；
- 超过上限必须显式报错，不能静默截断；
- 报错不得包含密码内容或文件路径。

### 5.5 前端文案

当前文案需要同时适配本地 `start` 和部署 Secret 两种来源，改为：

- 中文 hint：`从启动终端或部署管理员提供的安全通道获取。`
- 英文 hint：`Get it from the startup terminal or your deployment administrator's secure channel.`
- 中文 required：`请输入一次性初始化令牌。`
- 英文 required：`Enter the one-time setup token.`

错误文案继续使用统一的“初始化令牌无效”，不泄露来源或状态。

---

## 6. Kubernetes 交付形态

仓库当前只有示例 Manifest，没有正式 Helm Chart。本期先交付原生 Kubernetes 示例；以下 Helm values 形态作为后续 Chart 契约保留。

官方 Kubernetes Bootstrap Job 第一版只承诺 `storage.master_key.mode: key_slots`。File 模式的 `halro init` 需要生成并持久化可写 Master Key，不能把预置只读 Secret/CSI 投影误当成可写 Key Store；在生成后安全外送和持久保管协议完成评审前，不把 File 模式列为已支持的 Job 路径。

### 6.1 交互式初始化示例

对新 PVC，先运行只做系统初始化的 `halro init --if-needed` Job；确认成功后才创建以下 Deployment。不得只把现有 Manifest 的 `start` 改成 `serve` 而遗漏初始化阶段。

Secret：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: halro-bootstrap
  namespace: halro
type: Opaque
stringData:
  setup-token: REPLACE_VIA_SECRET_MANAGER
```

生产环境不得把真实值提交到 Git。示例中的占位值必须无法通过 Halro 的 Token 格式校验，防止误部署。
该 YAML 仅说明对象形态；正式部署优先由 ExternalSecret/CSI 等受控同步资源产生 Secret。直接创建原生 Secret 时，使用生成器写出的文件和 `--from-file`，不得用 `--from-literal` 把 Token 放入进程参数。

挂载：

```yaml
volumeMounts:
  - name: bootstrap
    mountPath: /run/secrets/halro
    readOnly: true
volumes:
  - name: bootstrap
    secret:
      secretName: halro-bootstrap
      optional: true
      defaultMode: 0440
```

`optional: true` 只解决 kubelet 在 Secret 删除后仍能构造新 Pod 的问题。首次初始化时文件缺失仍由 Halro 根据“无管理员 + 配置了 Token 文件”拒绝启动。以 UID/GID 65532 运行的实际镜像必须在部署验收中读取该文件；不同 CSI 驱动的 owner/group/mode 行为不能仅凭 Manifest 推断。禁止使用 `subPath`。

配置指向：

```yaml
admin:
  setup_token_file: /run/secrets/halro/setup-token
```

主容器运行：

```yaml
args: ["serve", "--config", "/etc/halro/config.yaml"]
```

### 6.2 自动化 Bootstrap Job 示例

交付以下资源骨架：

- `deploy/kubernetes/halro-bootstrap-job.yaml`；
- `deploy/kubernetes/halro-bootstrap-verify-job.yaml`，顺序执行离线 Doctor 与 `audit verify`；
- `deploy/kubernetes/halro-bootstrap-default-deny-network-policy.yaml`，由目标集群叠加 DNS、Workload Identity 与 KMS 精确出站规则；
- Secret 文件挂载示例；
- Job 完成检查命令；
- 后续启动 `halro serve` 的 Deployment；
- Secret 删除、轮换和失败恢复说明。

部署顺序必须由 CI/CD、独立的 install-only GitOps 阶段或人工受控步骤保证。Kubernetes Deployment 本身没有“等待另一个 Job 成功后再创建”的通用依赖语义，普通的每次同步 PreSync hook 也会在升级时重复执行，不可直接作为 bootstrap 生命周期。

标准状态机：

1. 首次安装不创建主 Deployment；维护场景则先暂停会自动重建 Deployment 的 GitOps reconciliation；
2. 将主 Deployment 缩容至 0，并等待所有 Halro Pod 删除、数据卷 detach/可重新挂载；
3. 应用 install-only 默认拒绝 NetworkPolicy 和目标集群精确 allow policy，再创建 `completions: 1`、`parallelism: 1`、`restartPolicy: Never` 的 Bootstrap Job；
4. Job 的 init container 执行显式 `halro init --if-needed`，主 container 执行带稳定 operation ID 的 `admin bootstrap --if-needed`；自动化路径不再额外运行独立 Init Job；
5. 等待 Job `Complete`，记录固定结果；删除已完成 Job/Pod并等待 PVC 释放，但暂不删除密码 Secret；
6. 创建只挂载配置/PVC、不挂载管理员密码的 Verification Job：init container 运行 `halro doctor`，主 container 运行 `halro audit verify`。只有 Completion、目标 operation/audit event、pending Intent、Audit HMAC 链、trusted checkpoint 和 KMS 审计全部通过，才接受安装成功；
7. 将 Bootstrap/Verification Job 与 Secret 同步资源从 GitOps desired state 移除，删除完成 Pod和管理员密码集群投影；外部密码按组织首次登录/轮换策略保管；
8. 撤销 Bootstrap Job 使用的临时 KMS/工作负载身份，并确认 Runtime 身份只有审核后的 Primary 解密权限；
9. 创建或恢复只运行 `halro serve` 的主 Deployment，并等待 Ready 与管理员登录验证。

`ReadWriteOnce` 不是单写者 fencing：同一节点上的多个 Pod 仍可能同时挂载。CSI 支持时优先使用 `ReadWriteOncePod`；无论访问模式如何，Bootstrap Job 与 Runtime 都禁止重叠，Halro 数据目录锁是最后一道拒绝并发访问的防线。`storage.data_dir` 必须是 PVC 挂载点的子目录，例如挂载 `/var/lib/halro`、配置 `/var/lib/halro/data`，以保留同级 publication lock 和原子 rename 所需空间。

Job 必须设置有限的 `backoffLimit`、`activeDeadlineSeconds` 和 `ttlSecondsAfterFinished`，沿用 non-root、只读根文件系统、drop capabilities 与 seccomp 限制。管理员密码只挂载给 bootstrap container，不挂载给 init、verification container 或正式 Deployment；Job 不创建 Service，不接受入站流量。仓库提供 install-only 默认拒绝 NetworkPolicy，部署方必须叠加目标集群 DNS、Workload Identity（如 EKS Pod Identity Agent 或 IRSA STS）与精确 KMS endpoint 的必要出站，并拒绝 EC2 IMDS；所有 AWS 容器设置 `AWS_EC2_METADATA_DISABLED=true`，避免身份关联故障时回退节点角色。CLI 日志只输出稳定的非秘密结果状态和 operation ID。

Bootstrap Job 使用独立 ServiceAccount 和临时 KMS Lifecycle Role。运行时身份继续保持最小 Primary `Decrypt` 权限；初始化所需的 Primary/Recovery Encrypt/Decrypt 权限只授予 Job，完成后撤销，并通过云审计日志验证。两种身份都不需要 Kubernetes Secret API 权限，Secret 投影由 kubelet 或受控 CSI 完成。

若使用 Vault Agent 或 CSI，必须满足 init-first 就绪门槛，并验证文件对 UID/GID 65532 可读。需要 Kubernetes 身份令牌时，只向对应 init/sidecar 显式投影短期、限定 audience 的令牌，不得因此打开全 Pod 的默认 ServiceAccount Token 自动挂载。

### 6.3 未来 Helm 契约

未来 Helm Chart 应只接受已有 Secret 的名称和 key，不鼓励在普通 values 文件中放明文：

```yaml
adminBootstrap:
  mode: interactive # interactive | job | disabled
  existingSecret:
    name: halro-bootstrap
    tokenKey: setup-token
    passwordKey: admin-password
```

Chart 不应把 Secret 值渲染进 Pod 注解、Helm Notes 或 ConfigMap。若 Chart 自动生成随机值，应由专用 Job 写入 Secret，而不是 Helm 模板的随机函数，以避免 upgrade 时发生不可控变化。
Bootstrap 是 install-only 状态：完成后 values 中的 mode 必须提交为 `disabled` 并从 desired state 移除 Job/ExternalSecret，或保留一个名称不可变、永不在 upgrade 中修改的已完成 Job。不得同时配置 TTL 自动删除和让 GitOps 永久声明同一个 Job，否则控制器会循环重建它。

---

## 7. RBAC 与秘密管理要求

### 7.1 人员权限

建议角色拆分：

| 角色 | 日志 | Bootstrap Secret | 部署变更 | Admin UI |
| --- | --- | --- | --- | --- |
| 应用研发 | 按需只读或无生产权限 | 无 | 无 | 无 |
| SRE/平台部署者 | 按需 | 受控模板下不必读取明文 | 有 | 无 |
| 初始化审批人 | 不需要 | 仅能读取指定秘密 | 无 | 首次初始化 |
| 安全管理员 | 审计访问 | 管理/撤销 | 策略级 | break-glass |

如果直接使用 Kubernetes Secret，应只授予指定主体对指定 Secret 的 `get` 权限，不授予 namespace 级 `list`/`watch`。同时必须认识到：能够在该 namespace 创建任意 Pod 的主体通常可以通过挂载间接读取 Secret，因此工作负载创建权限也是秘密边界的一部分。

“部署者不读取明文”只在部署权限被限制为受审模板、准入策略禁止任意 volume、command、image 和 ephemeral container 变更，并且 Secret Store 工作负载身份不可被冒用时成立。否则 `pods/create`、`pods/exec`、`pods/ephemeralcontainers`、工作负载 patch、节点调试、PVC/VolumeSnapshot 和 Secret Store 身份操作权限，都应按等价 Secret 读取能力治理。

### 7.2 集群要求

- Kubernetes Secret 启用 etcd 静态加密；
- namespace 执行 Restricted Pod Security；
- 优先使用外部 Secret Store 与短期工作负载身份；
- Secret 只挂载给需要它的 container；
- 不使用环境变量交付 Setup Token 或管理员密码；
- 日志采集、崩溃报告和诊断包不得包含 Secret 文件内容；
- 初始化完成后撤销外部秘密，并删除不再需要的 Kubernetes Secret。

非回环 Setup 只允许通过 HTTPS。反向代理到 Halro 的链路必须在可信网络内或继续使用 TLS；Ingress、WAF、service mesh、APM 和错误采样不得记录 `/admin/api/v1/setup/admin` 的请求体、Token、密码或完整网络包。Token 只能位于 JSON 请求体，不得进入 URL、query string、Referer 或 tracing attribute。

### 7.3 禁止项

- 禁止新增 `GET /admin/api/v1/setup/token`；
- 禁止在健康检查、Setup Status、Metrics 或错误消息中返回 Token；
- 禁止将 Token 写入数据库以便“以后查看”；
- 禁止让 Halro ServiceAccount 获得 Secret 的 create/update/delete 权限；
- 禁止通过命令行参数直接传入 Token 或密码；
- 禁止为了初始化授予普通工程师 `pods/exec`、`pods/log` 或集群管理员权限。

### 7.4 审计责任

- Secret 的创建、审批读取、轮换和删除由 Secret Manager、Kubernetes Audit 和云审计记录；
- Halro 的 `admin.bootstrap` 事件包含 `source=generated|file|offline`、非秘密 operation ID 和目标用户名，不包含 Token、摘要、Secret 名称或文件路径；
- 管理员记录与 Audit Intent 必须在同一个 BoltDB 事务中提交；Intent 向 Audit 链的交付和 checkpoint 允许恢复重放；
- 同 operation ID 重放和 `halro audit verify` 必须通过 Completion 的 Audit Event ID 在认证后的 Audit 链中找到 action、target、operation ID 全部匹配的成功事件；pending 与已交付链中都不存在时返回歧义失败；
- 无效 Token 和限流拒绝只做有界聚合观测，避免攻击者通过失败请求制造无限 Audit 写入；
- Token 文件成功加载只记录来源和状态，不记录路径、长度或摘要；
- `admin reset-password` 与 `admin reset-mfa` 仍存在“身份变更后直接追加 Audit”的同类窗口；这是一项需要单独修复的已知安全缺口，在持久 Audit Intent 落地前不得宣称离线 break-glass 具备完整审计原子性。

---

## 8. 失败语义与运维流程

### 8.1 文件不可用

无管理员且配置了 `setup_token_file` 时，文件读取失败必须阻止 Admin/Gateway 开始服务。错误示例：

```text
halro: load admin setup token from configured file: file is not readable
```

不得回退为自动生成并写日志，否则一个 Secret 挂载故障会静默改变安全模型。

### 8.2 Token 泄露

首个管理员尚未创建：

1. 立即在 Ingress/NetworkPolicy 阻断 Admin Setup 入口，或将 Deployment 缩容至 0；仅删除/修改 Secret 不会撤销旧进程已经加载的值；
2. 创建新的版本化、不可变 Secret，更新工作负载引用；不依赖 kubelet 或 CSI 对原地 Secret 更新的最终一致传播；
3. 启动新 Pod，确认读取的是新版本，并在 TLS 通道下验证旧值被拒绝；
4. 停止 ExternalSecret/同步控制器对旧值的声明，再删除旧 Kubernetes Secret 和外部秘密；
5. 检查 Secret Manager、Kubernetes 和云身份审计记录。

首个管理员已创建：

- Setup Token 已不再有效；
- 仍需删除泄露副本和检查是否存在异常初始化 Audit；
- 如果管理员身份可疑，使用离线密码重置并使既有 Session 失效。

### 8.3 Secret 删除后的重启

应用层面，只要已有管理员，Halro 就不读取 `setup_token_file`；但这不代表 kubelet 或 CSI 一定能在 Secret 缺失时构造 Pod。

- 官方原生 Kubernetes Secret 示例使用 `optional: true`，因此 Secret 删除后仍能创建新 Pod，缺失文件由 Halro 根据数据库状态决定是否 fail-closed；
- 若使用 CSI/Injector，先从 GitOps desired state 移除 bootstrap volume、mount 和同步资源，滚动创建不再依赖它的新 Pod并完成登录验证，再撤销外部秘密；
- 若不使用 `optional: true`，原生 Secret 也遵循上述两阶段顺序；
- 验收必须真正删除并重新调度 Pod，不能只在同一容器中重启进程。

Secret 是否仍被应用读取，以及编排层是否能构造 Pod，是两个独立依赖。配置中的文件路径可以暂时保留，因为已有管理员时应用不会访问它；强制 volume 依赖必须先解除。

### 8.4 Bootstrap Job 失败

- Job 未完成时不得启动主 Deployment；
- 数据目录处于完整初始化状态但管理员未创建时，可以修复 Secret 后重新运行 Job；
- 部分初始化或密钥不匹配继续使用现有 fail-closed 诊断，不自动覆盖；
- Bootstrap Intent 为 pending 时只能由同 operation ID 按恢复协议继续；状态歧义返回 `ambiguous_partial_bootstrap`，不得把“已有管理员”当作成功；
- 若持久 Intent/Completion 尚未实现，官方 Job 必须设置 `backoffLimit: 0`，失败后人工诊断，不能宣称自动重试幂等；
- RWO 卷 attach/detach、数据锁冲突、Secret/CSI 未就绪或 KMS 身份未就绪都属于分类失败，不通过增加并发 Pod 重试；
- 不通过启动一个临时 `halro start` Pod 绕过失败。

### 8.5 Break-glass

离线密码或 MFA 重置只能在主服务停止、数据目录独占锁可获得时执行。生产操作必须关联非秘密的审批单/incident ID；在 Halro 后续增加可归因的 `--operation-id`、`--reason` 和持久 Audit Intent 前，该关联由堡垒机/作业平台审计保存，不能宣称 Halro 自身已经形成原子审计闭环。调用者同时需要受审计的存储访问与 Master Key/KMS 解密授权。完成后验证旧 Session 全部失效、管理员可以登录，并撤销临时主机/KMS 权限。密码重置不得隐式重置 MFA。

---

## 9. 实施范围

### 9.1 后端与配置

已修改：

- `internal/config/config.go`
  - 增加 `Admin.SetupTokenFile` 与 `Admin.SetupTokenTTL`；
  - 绝对路径、Token TTL 上限和格式校验；
- `internal/config/default.yaml`
  - 增加默认空值和安全说明；
- `configs/config.example.yaml`
  - 增加中英文配置元数据；
- `internal/app/runtime.go`
  - 只在首次初始化需要时解析 Token 来源；
  - 支持有界读取外部文件、固定格式 Token、过期时间和摘要态保存；
- `internal/app/init.go`
  - 增加只供显式 `halro init --if-needed` 使用的入口，empty 状态对 File/Key-Slot 均执行显式初始化；
  - 保留 `start` 使用的现有 `InitializeIfNeeded` 语义，尤其不得自动初始化空 `key_slots`；
- `internal/app/admin_setup.go`
  - 使用显式 Token 来源；
  - 使用固定长度摘要做常量时间比较，并在管理员事务提交后使 Token 失效；
- `internal/app/admin.go` 与 `internal/store/bolt`
  - 为 Web/CLI 首个管理员创建增加持久 Bootstrap Intent/Completion；
  - 将管理员、operation ID 与 Audit Intent 原子提交，并实现恢复重放；
- `cmd/halro/main.go`
  - `serve` 对无法交付的 Setup Token fail-closed；
  - 增加 `--password-file`、`--if-needed`、`--operation-id` 与稳定结果/退出码；
  - 增加只写 `0600` 新文件的 `admin setup-token generate --ttl ... --output`；
  - Init、Admin bootstrap/reset、Token 生成及 Doctor/Audit verification 命令在接触密钥前应用进程 core-dump/dumpable 防护并 fail-closed；
  - `start` 只在生成模式下打印 Token。

Token 文件读取应封装为独立、小型函数并使用有界读取。Runtime 内部优先使用自有 `[]byte`，转换为摘要后以及初始化完成/Runtime 关闭时做 best-effort 清零并移除引用。实现不得声称能可靠清除 Go runtime、HTTP 解码器或字符串转换产生的所有历史副本；安全边界是秘密不被记录、持久化或回显，并尽量缩短自有副本生命周期。生产进程禁止 core dump，崩溃报告和 heap/profile 导出不得包含 bootstrap 凭据。

### 9.2 前端

已修改：

- `web/src/i18n/locales/zh-CN.ts`；
- `web/src/i18n/locales/en-US.ts`；
- `web/src/Setup.test.tsx`。

页面结构和 API 请求体不变，同时调整 `setupTokenHint` 与 `tokenRequired` 两处文案。

### 9.3 部署与文档

已修改或新增：

- 新增独立的 `deploy/kubernetes/halro-init-job.yaml`，并将 `deploy/kubernetes/halro-aws-kms.yaml` 的生产主容器从 `start` 改为 `serve`；部署流程必须先等待 Init Job 成功，不得只替换启动参数；
- 新增交互式 Secret 挂载示例；
- 新增 `deploy/kubernetes/halro-bootstrap-job.yaml`；
- 新增离线 Bootstrap Verification Job 与 install-only 默认拒绝 NetworkPolicy；
- 增加 Job 专用 ServiceAccount、临时 KMS Lifecycle Role、RWOP/RWO 编排和 GitOps install-only 状态机说明；
- 更新中英文 User Guide 和 Operator Guide；
- 增加 bootstrap Secret 泄露/轮换 runbook；
- 明确本地 `start` 与生产 `serve` 的用途边界。

---

## 10. 测试计划

### 10.1 配置测试

- 默认 `setup_token_file` 为空；
- 接受绝对路径；
- 拒绝相对路径；
- `setup_token_ttl` 默认 30 分钟，拒绝 0、负值和超过 24 小时的值；
- 默认配置、示例配置和字段元数据保持一致；
- 配置检查不要求首次启动时 Secret 文件已经挂载。

### 10.2 Runtime 与 HTTP 测试

- 无管理员 + 远程 Admin + 有效 Token 文件：正确要求并接受 Token；
- 错误信封、无效/过期 `expires_at`、空值、错误 Token 和超长输入被拒绝；两种来源的 Token 都只接受固定 49 字节规范；同一过期文件在进程重启后仍被拒绝，只有轮换新文件才恢复；
- 文件缺失、不可读、格式非法时启动失败；
- 文件模式不向 stdout、stderr 或结构化日志输出 Token；
- Token 到期后被拒绝，轮换后新 Pod 只接受新值；删除 Secret 但不重启时旧进程仍保持原加载状态；
- 管理员与 Audit Intent 事务提交后，运行时 Token 状态立即移除且 Session 失败不会重新开放 Setup；
- Audit append/checkpoint 失败、事务提交后崩溃和重启恢复均不会丢失 Intent 或产生第二个管理员；
- 已有管理员时，即使文件已删除也能启动；
- 显式配置 Token 文件时，loopback 初始化同样要求 Token；
- 两个并发正确请求仍只有一个能创建管理员；
- Setup Status 不泄露 Token 来源或文件路径；
- `serve` 在远程首次初始化却没有文件来源时 fail-closed；
- `start` 的生成模式保持现有交互行为。

### 10.3 CLI 测试

- `--password-file` 正常创建管理员；
- stdin 兼容不变；
- 超长、空文件和读取错误安全失败；
- 密码不进入错误输出；
- `init --if-needed` 对 File/Key-Slot 的完整实例幂等、对 empty 显式初始化、对残缺实例失败，同时不改变 `start` 的 Key-Slot 行为；
- `admin bootstrap --if-needed` 对相同 operation ID 返回 `already_completed`，不同 ID、用户名不匹配和无 Completion 的既有管理员 fail-closed；
- 在每个 durable write 之间注入终止，验证同 operation ID 可恢复且不会覆盖管理员或密码；
- Setup Token 生成命令产生固定 Token/绝对过期信封、以 `0600` 排他创建文件且不向标准输出/错误输出秘密；
- 离线数据锁冲突时 Job 明确失败。

### 10.4 前端测试

- `token_required=true` 时仍展示密码类型的 Token 输入框；
- 中英文 `setupTokenHint` 与 `tokenRequired` 新文案一致；
- 提交、错误和已初始化跳转行为不变。

### 10.5 部署验证

- 在临时 Kubernetes namespace 完成交互式 Secret 初始化；
- 验证 `kubectl logs` 中不存在 Token；
- 先运行 Init Job，再用 `serve` 完成交互式初始化；
- 删除 bootstrap Secret 后真正删除并重新调度 Pod，实例仍可登录；CSI/Injector 形态先移除挂载依赖再撤销；
- 运行 Bootstrap Job 状态机，确认 Job 与主 Deployment 从不重叠，主 Deployment 使用 `serve` 正常启动；
- 验证 RWOP 或 RWO + 数据锁、PVC attach/detach、锁冲突、Secret/CSI 未就绪和 KMS 身份未就绪的分类失败；Verification Job 必须同时通过 Doctor 与 `audit verify`；
- 重跑相同 operation ID 的 Job 返回确定状态，不同 ID 失败且不修改管理员密码；
- 验证 Bootstrap Lifecycle Role 已撤销、Runtime Role 只有所需 KMS 权限，并检查云审计记录；
- 对 Ingress/WAF/APM、Job、应用和失败请求日志执行 canary 泄露检查；
- 验证普通应用工程师 RBAC 无需 `pods/log`、`pods/exec` 或 Secret 权限。

实施期间遵循仓库验证策略：迭代时先运行受影响的 Go package、CLI 测试和 `web/src/Setup.test.tsx`；推送前只运行一次完整 frontend gate、`go test ./... -count=1`、生产构建及嵌入 bundle drift 检查。

---

## 11. 分阶段交付

### 阶段 1：安全输入原语

- 增加 `admin.setup_token_file`、固定格式 Token 和过期时间；
- 增加 CLI `--password-file` 与安全 Token 生成器；
- 将首个管理员、Bootstrap Completion 与 Audit Intent 改为可恢复的原子提交协议；
- `serve` 对没有安全 Token 来源的远程首次初始化 fail-closed；
- 更新前端文案和运维文档。

阶段 1 完成后，远程生产环境已经不需要日志权限。

### 阶段 2：Kubernetes 正式路径

- 为现有 AWS KMS Manifest 增加显式 Init Job，再将 Runtime 改为 `serve`；
- 增加交互式 Secret 挂载示例；
- 增加带 operation ID、专用 KMS 身份和明确 PVC fencing 的幂等 Bootstrap Job；
- 增加顺序执行 Doctor/Audit 校验的 Verification Job、默认拒绝出站基线与节点 IMDS 禁用；
- 增加 install-only GitOps 生命周期与 Secret/CSI 移除顺序；
- 增加 Secret 轮换 runbook。

### 阶段 3：后续增强

- 正式 Helm Chart 的 `existingSecret` 契约；
- 外部 IdP/OIDC 管理员组 bootstrap；
- 可选的本地管理员禁用与 break-glass 策略。

阶段 3 不阻塞本方案前两阶段。

---

## 12. 验收标准

本方案实施完成需同时满足：

1. Kubernetes 生产示例从头部署时，无需读取 Pod 日志即可完成首次初始化；
2. 默认生产路径的任何日志中都不存在 Setup Token 或管理员密码；
3. 交互式模式支持外部 Secret 文件，自动化模式支持一次性离线 Job；
4. Halro ServiceAccount 不新增 Kubernetes Secret API 权限；
5. 首个管理员、Bootstrap Completion 与 Audit Intent 原子提交；Audit 交付失败可恢复且不产生未审计管理员；
6. 同 operation ID 重试返回确定成功，其他 ID 和歧义状态 fail-closed，不覆盖管理员或密码；
7. Token 固定格式、每实例唯一并限时有效；轮换流程承认旧进程不会因 Secret 删除而自动撤销；
8. 原生 Secret 删除或 CSI/Injector 挂载解除后，真正重新调度的 Pod 可以正常启动；
9. Init/Bootstrap Job 与 Runtime 不并行，PVC、数据锁和 GitOps 状态机经过故障注入；
10. Bootstrap Job 使用独立临时 KMS 身份，完成后撤销，Runtime 身份权限不扩大；
11. 现有本地 `make start` / `halro start` 首次体验不退化；
12. Setup Token 的读取、比较、过期、best-effort 清理和错误路径均有自动化测试；
13. Ingress/WAF/APM、Job 与应用日志通过端到端秘密 canary 检查；
14. 中英文用户与运维文档清楚区分 `start`、`serve`、Init Job 和 Bootstrap Job；
15. 完整验证门禁通过，生成的 `internal/webui/dist` 与前端源代码一致。

---

## 13. 多角色评审裁决

| 评审角色 | 阻断问题 | 本文裁决 |
| --- | --- | --- |
| 安全架构 | 管理员与 Audit 非原子；Token 熵不可由长度推断；Secret 删除不撤销旧进程；信任边界不完整 | 引入持久 Audit Intent/Completion、固定 CSPRNG Token 规范与 TTL、摘要比较、明确轮换状态机和代理链防泄露要求 |
| Kubernetes/SRE | `serve` 不初始化新 PVC；non-optional Secret 删除后 Pod 无法重建；RWO 不是 fencing；GitOps 会重跑 Job；KMS 初始化权限过大 | 增加显式 Init Job、optional Secret/两阶段卸载、install-only 状态机、RWOP 优先与数据锁、Job 专用临时 KMS 身份 |
| Go/CLI 架构 | `init --if-needed` 不能复用 `start` 路径；简单“已有管理员即成功”会掩盖半完成；探测 stdin 会阻塞；Go 无法保证完全零化 | 分离显式 CLI 初始化入口、operation ID 幂等协议、flag 明确选择密码来源、只承诺 best-effort 清理 |
| 产品与兼容性 | 只写“找部署管理员”会破坏本地 `start` 指引；`serve` 新 fail-closed 行为需要升级说明 | 使用兼容两种来源的文案，保持 HTTP 契约与本地体验，并把零管理员远程 `serve` 的变化列为显式兼容性事项 |

在前三类阻断项落地并通过故障注入前，自动化 Bootstrap Job 不得标记为生产推荐。OIDC/SAML 管理员组、正式 Helm Chart，以及 Kubernetes File Master Key 初始化协议继续作为后续独立工作，不阻塞 Secret 文件交付基础能力。

---

## 14. 实施记录

2026-09-17 已完成阶段 1 与阶段 2 的仓库交付：固定格式/TTL/摘要态 Setup Token、文件来源与 `serve` fail-closed、首管理员/Audit Intent/Completion 单事务、operation ID 幂等恢复、CLI 密码文件与显式初始化、前端文案、Kubernetes Init/Bootstrap Job、Secret 生命周期文档及泄露 runbook 均已落地。

自动化验证已通过：

- `go test ./... -count=1`；
- 受影响并发路径的定向 `go test -race`；
- `go vet ./...`；
- 前端 TypeScript typecheck、44 个测试文件共 614 项测试、生产构建与浏览器产物秘密扫描；
- Kubernetes 示例 YAML 解析与仓库差异检查。

Kubernetes admission、PVC attach/detach、CSI 文件属主、真实 KMS 身份、NetworkPolicy 和 Secret 删除后重新调度属于目标集群特性，不能由无集群的仓库测试伪造为已通过。部署者必须按 `deploy/kubernetes/README.md` 在临时 namespace 完成第 10.5 节的环境验收后，才可把对应环境标记为生产就绪。阶段 3 仍按后续增强处理。

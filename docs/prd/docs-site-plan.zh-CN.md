# Halro 开发者文档站方案

状态：**P0 已实施并合入网站仓库，P1／P2 未开始**（v2，经五角色评审重写）
更新日期：2026-08-20
归档日期：2026-08-20
承载仓库：`/Users/ziy/Code/ClayCosmos/Halro-website`（Astro 官网），实现见该仓库 `d6db4de`
内容来源：本仓库 `docs/`、`internal/compatibility/`、`internal/config/`

---

## 归档说明（2026-08-20）

归档时逐条对两个仓库的代码核实，而不是照抄正文。正文以下部分保持原样——它记的是决策
与证据；本节只说明哪些落了地、哪些没有、哪些被实现推翻。

### 已落地

- **Starlight 接入**（§4.1、§4.2）：`@astrojs/starlight@0.41.7` + Astro 7.2.1，构建通过。
  `astro` 与 `@astrojs/check` 已从 `"latest"` 钉成 `^7.2.1` / `^0.9.10`，即 §4.1 第 2 条
  「一行改动、不改迟早炸」的那条。
- **挂载形态与 §4.1 第 3 条的设想不同**：Starlight 只在站点根注入 `[...slug]`，所以内容放在
  `src/content/docs/docs/`，slug 自带 `/docs` 前缀；首页是显式路由，胜过注入路由。
- **13 个内容页**（方案定的是 12–15）：对象模型、我拿到了一个 Key、我要装一套、streaming、
  retries、五个 compatible 端点参考、实验性端点合并页、认证与错误、文档首页。
- **API 参考页由契约生成**（`scripts/generate-api-pages.mjs`）。契约不带标题与示例，这两样来自
  手写表 `scripts/api-endpoints.mjs`；没有条目的端点不生成示例段，而不是硬凑三个 Tab。
  §5.4 的四条渲染口径都实现了：端点成熟度不提升 profile 成熟度、未声明限制的 profile 渲染成
  「未声明」而不是「支持」、组合级拒绝以脚注挂在它影响的字段行上、SDK 矩阵标注为协议桩矩阵。
- **link checker 进了构建**：`bun run build` 末尾跑 `scripts/check-links.mjs`，含 trailing slash
  归一、CJK 锚点 `decodeURIComponent` + NFC、`publicDir` 资产视为合法目标。首次运行抓到四条死链。
- **`site` 补回了 `astro.config.mjs`**（§1.1 指出它缺失），但填的是占位域名。
- **契约快照已入库**：`src/data/endpoint-manifests.json` 与 `src/data/contracts.meta.json`
  （`source_commit` = `7117fdc`）。核实：该快照的 sha256 与本仓库当前
  `docs/compatibility/endpoint-manifests.json` 相同，`7117fdc..HEAD` 之间契约未再变——今天不漂，
  是因为上游正好没动，不是因为有门禁。

### 未落地，或被实现推翻

1. **英文被刻意关掉，推翻 §0.1 第 4 条的「中英同写」。** 理由记在 `d6db4de`：只声明 locale 而
   没有英文正文，会产出 `lang="en"` 的中文页，Pagefind 随后把这些中文按英文索引，中文查询在这些
   页上返回空。这是对方案的推翻，不是遗漏。
2. **§5.1 的契约补字段没做。** `internal/compatibility/manifest.go:77` 的 `RequestFields` 仍是
   `[]string`，全树没有 `ManifestField`。API 页因此停在 §9 描述的 P0 形态：字段名清单 + 被拒字段
   + 请求头 + 偏差 + 覆盖矩阵。§12.2 的 `profile_revision` 是否随结构变更 bump 也仍未评审。
3. **§5.2 的同步链路不存在。** 网站侧没有 `scripts/sync-halro-contracts.mjs`（快照是一次性拷入
   的），本仓库也没有 `scripts/check-docs-site-contract-sync.sh`。方案里「触发器放 Halro 侧，不靠
   人记」这条没有执行者。
4. **§5.5 的分级漂移门禁、§10 的机器断言全部未建**：`check:api` 的四态互斥覆盖断言、`null` 与
   `[]` 区分、产物密钥扫描、搜索传输量、a11y、hreflang／sitemap，一条都没有。落地的只有
   `check:links`。
5. **§8 的三件前置只做了半件**，且顺序与 §9 的 P-1 相反——P0 先上线了：`site` 补回但域名未定
   （占位值），`.github/workflows/` 仍不存在（网站仓库依旧全部直推 main），发布路径未定。
6. **P1 页面一页未写**：TLS 与反向代理、`config.yaml` 参考（§6 的 15 个 section 与
   `tools/configdoc`）、预算限流与用量核算、运维手册、兼容性矩阵、变更日志入口；SDK 示例
   （Python／Node／Go／Java／Anthropic SDK）也未补。
7. **§12.4 未处理**：`docs/README.md` 与站点索引是否并存，仍未定。

### 对本仓库的影响

本方案落在本仓库的改动只有 §5.1 一项，而它没有实施。**不需要重新初始化数据目录。**

---

## 0. 决策

要做的是一个**开发者文档站**，回答三件事：这套系统怎么用和怎么配、API 怎么调、有没有能直接抄的例子。参照形态是 DuckDB Docs 与 Claude Platform Docs。

### 0.1 已拍板

1. **站点建在 `Halro-website` 仓库**，不进 Halro 二进制。文档作为 `/docs/*` 一组新路由并入现有 Astro 站。
2. **用 Starlight**（`@astrojs/starlight@0.41.7`，peer `astro: ^7.0.2`，站点在 7.2.1，兼容）。不自研导航/TOC/搜索/i18n/主题。
3. **范围砍到 12–15 页**。15 个 experimental 端点合并成一页表格，不做 15 个独立页面。
4. **中英同时写，否则只做中文**。不做「先中文、英文以后补」——单人项目里那等于英文不做，而「隐藏未翻译条目」会让英文读者永远看不见自己少了什么。
5. **先在 Halro 侧给 `EndpointCompatibilityManifest` 补 `type` / `required` / `description`**，让生成管线真正成立（§5）。

### 0.2 v1 被推翻的决策，与推翻它的证据

| v1 决策 | 结论 | 证据 |
| --- | --- | --- |
| 「零新增运行时依赖」，自研搜索/主题/组件 | **推翻** | Starlight 今天就兼容；自研清单（bigram 分词、link checker、nav 校验、主题防闪烁、8 个组件）是把依赖写成自己的代码，由一个人独担维护 |
| 搜索索引 gzip ≤ 150 KB「宽裕」 | **推翻** | 真语料实测：60 页 × 3000 汉字的 bigram 索引 = raw 884 KB / **gzip 292 KB**；即使每页压到 1500 字也已 158 KB。原数字差近 2 倍 |
| 内建 `i18n` 配置 + 手写 `/docs/en/` | **推翻** | POC 实测：`/docs/en/*` 里 `Astro.currentLocale` 返回 `zh-CN`，`getRelativeLocaleUrl('en-US', …)` 返回 `/en-us/docs/…`。那段 config 一行都用不上 |
| API 参考「由契约生成，杜绝手抄漂移」 | **部分推翻** | `request_fields` 是裸字符串数组，无类型/必填/描述；`unsupported_request_fields` 只覆盖 4/20 端点；`sdk_matrix` 5/20 非空；`stream_events` 3/20 非空。按原设计生成，20 页里 16 页的核心段落是空的 |
| 50+ 页的完整 IA | **推翻** | 现有中文素材 `user-guide.zh-CN.md` 仅 23.3 KB，目标成品约 172 KB（7 倍）。实测工作量 40–65 人日，v1 隐含 10–15 人日 |
| 契约漂移「告警不阻断」，理由是「阻断会让改样式也变红」 | **理由推翻，结论收窄** | 稻草人：`paths: [src/data/**]` 过滤的 required check 改 CSS 永远不会红。见 §5.4 的分级门禁 |
| 「§1.2 内容基本已经存在」 | **推翻** | 见上；且现存内容未经核对——`user-guide.zh-CN.md:475` 的排障表把 `price_unavailable` 记作 `409`，而代码有三个返回点：`internal/gateway/service.go:531` 是 **503**（有效价格不可用），`:2238`、`:2286` 是 **409**（候选定价无法选出 / 成本治理要求已知价格）。不是错，是不完整——而「同一个错误码有两种状态」正是排障时最需要说清的那类 |

### 0.3 v1 中被实证确认成立的部分

- `publicDir: './docs'` 与 `src/content/docs/` 并存无冲突（POC 同时构建成功，`dist/` 顶层 = `_astro docs favicon.svg index.html story`）。保留，不改名，保住已发布的 `/articles/*`。
- 网站仓库现状盘点（BaseLayout 47 行、Header 19 行、global.css 264 行、Astro 7.2.1、依赖仅三个）逐条核实无误。
- `gw_xxxxxxxxxxxxxxxx` 是安全占位符：真实 Gateway Key 是 `gw_` + base64url(32 字节) = 43 字符，占位符长度不对，过不了 `internal/auth/gatewaykey.go` 的格式校验。但文档应顺带写明真实长度，否则会有人按 16 位截断后来报 bug。

---

## 1. 现状盘点

### 1.1 网站仓库

| 路径 | 内容 | 与文档站的关系 |
| --- | --- | --- |
| `astro.config.mjs`（7 行） | `output: 'static'`、`publicDir: './docs'`、`prefetch: true`。**没有 `site`** | `site` 曾在 `c16660d` 加入、`90bed81` 删除。Starlight 的 sitemap 与 canonical 都依赖它，必须补回 |
| `src/layouts/BaseLayout.astro` | 硬编码 `lang="zh-CN"`（`:22`）、`theme-color #080b0a`（`:26`）、`og:locale zh_CN`（`:32`）、中文 skip-link（`:44`） | 文档页改用 Starlight 自己的布局，**不复用它**；首页/story 继续用 |
| `src/styles/global.css`（264 行） | 暗色令牌 `--bg #080b0a`、`--lime #cfff4a`、`--mint #83e6b3`、`--mono`/`--sans` | 作为品牌来源映射进 Starlight 的 `--sl-color-*`（§4.3） |
| `package.json` | `astro`/`@astrojs/check` 写作 `"latest"`；`packageManager: bun@1.3.14`（本机实际 1.2.22） | **必须先钉版本**（§4.1） |
| `.github/` | **不存在**。全仓 5 个 commit，全部直推 main，无一次 PR | §5.4、§10 的全部门禁没有执行者，必须先建（§8） |
| 发布链路 | **不存在**。`.openai/hosting.json` 与 `scripts/stage-worker.mjs`（Cloudflare Worker + Static Assets 形态）在 `44aa16e` 加入、`c16660d` 一并删除；`dist/` 在 `.gitignore` | 见 §8 |

> v1 把 `44aa16e` 写成删除那次，是写反了：它是**新增** `stage-worker.mjs` 的 commit，删除是 `c16660d`。

### 1.2 Halro 侧素材的真实体量

| 来源 | 体量 | 可用性 |
| --- | --- | --- |
| `docs/guides/user-guide.zh-CN.md` | **23.3 KB**、510 行、13 节 | 唯一的中文素材。控制台部分（§3.2/§3.3/§5/§9）加起来不到 4 KB，等于从零写 |
| `docs/guides/operator-guide.md` | 47 KB，**英文** | 安装/配置/运维的底稿 |
| `docs/compatibility/endpoint-manifests.json` | 20 端点 | 见 §5，补字段前只对覆盖矩阵与偏差列表权威 |
| `internal/config/default.yaml` | 15 个顶层 section | 见 §6 |
| `web/src/pages/DeveloperPage.tsx` | 566 行 | **只能搬 curl 模板**（`:543-551`，已是 `Bearer $HALRO_API_KEY`）。其余生成的是**裸 HTTP** 片段（`fetch`/`requests`/`net/http`），不是 SDK 示例；只覆盖 `responses`/`chat`/`embeddings` 三个端点；Go 非流式片段到 `http.DefaultClient.Do(req)` 就结束，不可运行 |

**结论修正**：不是「内容基本已经存在」。存在的是**素材**，且素材本身未经核对。砍到 12–15 页之后，写作仍是本方案的主要工作量。

---

## 2. 读者与范围

| 读者 | 目标 | 入口 |
| --- | --- | --- |
| 应用开发者（**只有** Gateway Key 和一个模型别名） | 5 分钟内跑通第一次调用，遇到 4xx 能自救 | 「我拿到了一个 Key」 |
| 部署者 / 运维 | 装起来、配 TLS、备份、接监控 | 「我要装一套」 |

v1 的「快速开始」把两条路径混成一页，并映射到 `user-guide.zh-CN.md` §2/§3.1 —— 而那两节从 `git clone` + `make start` 开始，讲 `master.key` 权限和 `/admin/setup`，持 Key 的人一条都用不上。**两条首屏路径必须分开。**

**明确不做**：文档站内的可交互 API 试跑（控制台 `/admin/developer` 已用真实 Key 与真实路由做了这件事，静态站做个假的只会误导）。

---

## 3. 信息架构（12–15 页）

```
开始使用
  Halro 是什么与核心对象模型     Credential → Provider → Deployment → Route → Project → Gateway Key
  我拿到了一个 Key（5 分钟）      面向应用开发者
  我要装一套（5 分钟）            面向部署者
安装与配置
  安装与首次初始化                二进制 / Docker / K8s 单副本约束
  TLS 与反向代理                  形态 A / 形态 B（推荐 B）
  config.yaml 参考                15 个 section 全覆盖，见 §6
API
  认证、请求头与错误              含 429 的六个成因、Retry-After、WWW-Authenticate、/v1/models 的说明
  Chat Completions                /v1/chat/completions
  Embeddings                      /v1/embeddings
  Responses（无状态）              /v1/responses，含 23 条被拒字段
  Anthropic Messages              /v1/messages（+ count_tokens 同页）
  实验性端点一览                  一页表格覆盖其余 15 个端点
  示例集                          curl / Python / Node / Go / Java / Anthropic SDK
运行与治理
  预算、限流与用量核算            RPM/TPM/并发/预算/Token Guard/脱敏/审计，一页
  运维手册                        备份恢复 / Master Key / Metrics 与告警 / 升级回滚 / 排障
参考
  兼容性与 Provider 覆盖矩阵
  变更日志与升级说明              链接 CHANGELOG.md / SECURITY.md
```

**归属规则**（v1 缺这条，是评审判定「半年后最先腐化」的地方）：同一个语义只在一页展开，其余页只链接不复述。RPM/TPM/预算/Token Guard 的**权威页是「预算、限流与用量核算」**；控制台里怎么点属于「运维手册」；配置项字段属于「config.yaml 参考」。三页互链，不重写。

---

## 4. 技术方案（Starlight）

### 4.1 接入与必须先验证的三件事

动工第一步是一个半天的接入验证，通过之后才写内容：

1. **peer 依赖会切换 markdown 管线**。Astro 7.2.1 把 `@astrojs/markdown-remark` 列为**可选** peer（`node_modules/astro/package.json:155-162`），默认管线用 `@astrojs/markdown-satteri@0.3.5`；而 Starlight 0.41.7 把 `@astrojs/markdown-remark: ^7.2.0` 列为**必需** peer。装 Starlight 会把 remark 管线拉回来。这是真实的行为变更，不是零成本 —— 现有 `docs/articles/*.md` 与首页不受影响（它们不走 content 管线），但必须实测确认构建通过。
2. **钉版本**。把 `astro` 与 `@astrojs/check` 从 `"latest"` 改成 `bun.lock` 里已锁定的 `^7.2.1` / `^0.9.10`。不做的话，一次不带 `--frozen-lockfile` 的 `bun install` 就可能跳大版本，而整个内容层以 Astro 7 的 API 为前提。这是全方案里「一行改动、不改迟早炸」的唯一一条。
3. **`/docs` 子路径挂载**。Starlight 默认占据站点根，本站根已被首页占用，需要确认它挂在 `/docs` 下的配置形态与首页共存。

### 4.2 Starlight 提供什么、还要自研什么

| 能力 | 来源 |
| --- | --- |
| 三栏布局、侧栏导航、右栏 TOC、面包屑、上一页/下一页 | Starlight |
| 搜索 | Pagefind（`pagefind@^1.5.2`，内置 CJK 分词、按需分片加载）。**v1 那个 292 KB 单体索引的问题随之消失** |
| i18n：`defaultLocale` 回退、缺译提示条、语言切换 | Starlight。**同时解决 v1 里 §0 与 §4.7 那两套互斥的缺译策略** |
| 亮/暗主题与切换、双 Shiki 主题 | Starlight（expressive-code）。v1 的单 `shikiConfig.theme` 会输出**内联绝对颜色**，不响应 `data-theme`，是死路 |
| 代码块多语言 Tab、复制按钮、Callout | Starlight 内置 `<Tabs>` / `:::note` |
| 侧栏与内容互查 | sidebar 配置即校验 |
| **`EndpointTable` 组件** | 自研（§5.3） |
| **link checker** | 自研，约 150–250 行。真实误报源是 trailing slash 归一、CJK 锚点（id 是字面中文 `#认证与请求头`，href 里被 percent-encode，必须 `decodeURIComponent` + NFC）、github-slugger 对重名标题追加 `-1`。`prefetch: true` 不产生额外 href，与此无关 |

**接受的代价**（如实记录）：29 个直接依赖（含 pagefind 的平台原生二进制、`astro-expressive-code`、`@astrojs/mdx`）；v1 的「首屏 JS ≤ 10 KB」作废，Pagefind 是懒加载 wasm+js，量级 50–100 KB；品牌对齐从「直接复用令牌」变成「覆盖 `--sl-color-*`」，约一天。

### 4.3 品牌对齐

把 `global.css:1-22` 的令牌映射进 Starlight 的 `--sl-color-*`：`--lime #cfff4a` 作 accent、`--bg #080b0a` 作暗色背景、`--mono`/`--sans` 作字体。浅色一侧需要新增一个 `--on-accent` 令牌 —— 现有 CSS 有 3 处把 `var(--bg)` 当前景色用（`global.css:31` `.skip-link`、`:123`、`:177`），在浅色下会变成浅底浅字。

首页与 story 保持强制暗色，不参与切换：global.css 有 28 处不走令牌的颜色字面量（8 个 hex + 20 个 `rgba()`，分布在 17 行），其中 22 处集中在首页重视觉区。这等于**永久放弃**把主题切换扩到首页，是自觉的取舍。

### 4.4 i18n

- 中文 `zh-CN` 为 `defaultLocale`（无前缀），英文 `en`。用 Starlight 的 locale 机制，不手写路由、不启用 Astro 内建 `i18n` 配置。
- **中英同写**：一页的中英两份同时合并，不单独立英文批次。做不到就这页只做中文，并在侧栏用 Starlight 的缺译机制标注。
- 术语表固定中英对照（Gateway Key、Deployment、Route、Project、Token Guard、Redaction 一律不译），供两侧对齐。

> 内容目录用小写 `zh-cn/` / `en/`：glob loader 的默认 `generateId` 逐段过 github-slugger，`zh-CN/` 会被小写成 `zh-cn`，任何 `id.startsWith('zh-CN/')` 会**静默返回空数组**（不报错，页面全没）。Starlight 自带的 docs collection 同理。

---

## 5. 契约工作

### 5.1 第一步：在 Halro 侧给契约补字段（本方案最大的单项工程）

今天的 `EndpointCompatibilityManifest`（`internal/compatibility/manifest.go:69`）里，`RequestFields []string`（`:77`）是裸字符串数组。要让生成管线真正成立，改成携带语义的结构：

```go
type ManifestField struct {
    Name        string `json:"name"`
    Type        string `json:"type"`                  // string / integer / boolean / object / array<string> …
    Required    bool   `json:"required,omitempty"`
    Description string `json:"description"`
}
```

影响面（已核实，比预想小）：

- **包外没有运行时消费者**。`BuiltinEndpointManifests` 与 `RequestFields` 在 `internal/compatibility/` 之外没有任何 Go 引用，唯一的包外提及是 `internal/app/admin_provider_profiles_golden_test.go:17` 的一句注释。
- 要改的：`manifest.go` 的 struct 与 `Validate()`（`:94` 的 `len(RequestFields) == 0`、`:141` 的 `hasEmptyOrDuplicate` 对 `[]string` 的遍历、`:160-161` 建 `requestFields` 集合的地方）、20 个 manifest 的字面量定义、golden 快照、`manifest_test.go`、`manifest_portable_coverage_test.go`（`:87`、`:148` 用 `UnsupportedRequestFields` 做交叉校验）。
- **不需要重新初始化数据目录**：manifest 是编译期常量与文档 golden，不是持久化状态。
- 按 pre-1.0 的规矩**原地改**，不保留 `[]string` 的旧读法，不并存两种形态。
- 待评审：`profile_revision`（当前全为 1）是否随结构变更而 bump。这是契约变更，需要按 `docs/compatibility/README.md` 的口径做一次审慎评审，不能顺手带过。

**探路草稿已跑**（按 §5.3 骨架把 20 个 manifest 渲染成纯文本，脚本见 §12.1）。结论比预想好：

| | 端点数 | 请求字段 | 被拒字段 | 响应字段 |
| --- | --- | --- | --- | --- |
| `compatible` | 5 | **92** | 35 | 58 |
| `experimental` | 15 | 45（每页平均 **3.0**） | 0 | — |

- 补字段的收益**只集中在 5 个 compatible 端点**：`/v1/responses` 34 个请求字段 + 23 个被拒字段是最大的一页，`/v1/messages` 26 + 9 次之，`/v1/chat/completions` 18，`count_tokens` 9，`/v1/embeddings` 5。
- 15 个 experimental 端点平均每页只有 3 个请求字段、1 个响应字段、`sdk_matrix` 全为 `null`、`profile_coverage` 多为单个 profile 且沉默 —— 它们**本来就要合并成一页表格**（§3），不展开，也就不需要字段描述。
- 因此实际工作量是 **5 个端点、150 条 `type`/`required`/`description`**（92 请求 + 58 响应），约 **2–3 人日**，不是原估的 200 条 / 3–5 人日。收益归两个仓库共享（主仓的兼容性测试也能用）。

同一份草稿还确认了 5 个 compatible 端点**在补字段前就不是空壳**：`documented_deviations` 每个 3 条以上、`request_headers` 齐全、`/v1/chat/completions` 的 `profile_coverage` 有 10 个 profile 其中 6 个声明了限制或变换。空壳的是那 15 个 experimental —— 而它们已经被合并掉了。

**判断：值得做，且可以提前**。见 §9 的分期调整。

### 5.2 契约同步

`scripts/sync-halro-contracts.mjs`：从本地 Halro 路径或 GitHub raw 读 `endpoint-manifests.json`，写入 `src/data/endpoint-manifests.json` 与 `src/data/contracts.meta.json`（`source_ref` / `source_commit` / `sha256` / `synced_at`）。页脚渲染「本页字段表由 Halro `<commit 短哈希>` 的兼容性契约生成」。

同步时的构建期断言（v1 的「含 id/method/path/status 等必需键」太弱，以下三条今天全部成立，正好可当基线）：

1. `provider_profiles` 与 `profile_coverage[].profile_id` 集合相等（20/20 成立）；
2. 每个 `unsupported_request_fields` 的字段名必须命中同端点 `request_fields`（零孤儿）；
3. `status` 与 `profile_coverage[].status` 取值只允许 `{compatible, experimental}`。

**同步的触发器**放在 Halro 侧，不靠人记：`endpoint-manifests.json` 是 `internal/compatibility/manifest_test.go` 的 golden，改契约必然改它、必然跑 `go test`。照搬 `scripts/check-dependency-license-review.sh` 那套（`git hash-object` 求哈希、在文件里 grep 期望值、不一致 exit 1，由 `.github/workflows/ci.yml:36-37` 调用），加一个 `scripts/check-docs-site-contract-sync.sh`，契约变更时同一条 CI 步骤失败并打印该跑的同步命令。

### 5.3 API 页骨架

补字段之后每页的固定结构：

1. 标题、方法与路径、`semantic_operation`、成熟度徽章（取端点 `status`）、`state_semantics`（**是整句不是短标签**——`halro.async.cancel.v1` 的值是「always fails closed because Bedrock has no cancellation operation…」，与徽章并排会被截断，而截断掉的正好是 fail-closed 这个语义）
2. **`documented_deviations`**（20/20 全非空，每端点 2–9 条，是覆盖最好的字段，理应靠前）
3. 请求示例（curl 必有，SDK 按端点类型可选 —— 对 `GET /v1/files/{id}`、`DELETE /v1/files/{id}` 这类端点五个 Tab 各写一段是纯噪音）
4. **`request_headers`**（20/20 有；`anthropic.messages` 有 5 条含 `Halro-Route-Mode`、`anthropic-beta`，媒体端点带 `Idempotency-Key when creating a resource`）
5. 请求字段表（补字段后带类型/必填/描述）
6. **`rejected_request_fields`**（4/20 有；`/v1/responses` **23 条**、`/v1/messages` 9 条。这是「我照 OpenAI 文档发的请求为什么 400」的唯一答案，v1 整个漏掉了）
7. 响应字段表、`stream_events`（仅 3/20 非空）
8. Provider 覆盖矩阵 + `declared_transforms`
9. `evidence` + SDK 黑盒矩阵

### 5.4 渲染口径约束（不可协商）

`docs/compatibility/README.md` 定了三条口径，而 §5.3 骨架按字面实现会**自然生成**违反它们的页面。以下是硬约束：

- **第 9 项标题必须是「SDK 黑盒协议桩矩阵」**，不能是「SDK 支持」。固定附一句：该矩阵只证明这些 SDK 版本能序列化请求并消费 Halro 声明的响应/流形状，不构成端到端网关或真实 Provider 证据（README:38-42 明写它不经过真实 Runtime router、Gateway Key 认证、路由选择、能力过滤、脱敏、预算核算）。`evidence` 必须与矩阵同屏。
- **第 8 项表头禁止用「支持的 Provider」**，用「可路由的 Provider Profile 及各自成熟度」。每行徽章取 `profile_coverage[].status`，**绝不复用端点 status**。数据里真有一处分歧：`/v1/embeddings` 端点 `compatible`，而 `bedrock.runtime.invoke.titan-embed-text-v2.v1` 的 coverage 是 `experimental`；不一致时强制插一行提示。页面任何位置不得出现模型名。
- **`declared_transforms` 必须与字段表同格渲染**（挂在对应 profile 列的脚注），不能只在下一节出现。原因：字段粒度表达不了组合级拒绝 —— `bedrock.mantle.openai.responses.v1` 的「带工具的流式请求在 Provider I/O 前被拒」会让字段表把 `tools` 和 `stream` 双双显示为支持。
- **字段 × Profile 有四态**：profile 未列该字段（支持）、在 `unsupported_request_fields` 里（不支持）、在 `rejected_request_fields` 里（门面层拒绝）、profile 不在 `profile_coverage` 里（未声明）。四态划分必须是**构建期断言**（互斥且覆盖），不是人工抽查。59 条 coverage 里 **30 条**既无 `unsupported_request_fields` 也无 `declared_transforms`（51%）；只看 `unsupported_request_fields` 一项缺失则是 37 条（63%）。把这种沉默渲染成「全字段支持」是系统性错误，人眼抽不出来。表格需要固定图例：沉默 = 契约未声明限制，不等于已验证支持。
- **缺失态必须显式渲染，但「`null` vs `[]`」这个区分在数据里不存在**：全 manifest 扫描，空数组 **0 个**；`sdk_matrix` 15 个是 `null`，`stream_events` 17 个是 **key 缺失**，`rejected_request_fields` 16 个 key 缺失。也就是说 README:55-56 对 `/v1/rerank`、`/v1/async/invocations` 的刻意表态（「在有诚实的客户端兼容套件之前，SDK 矩阵保持为空」）与另外 13 个「尚未声明」在契约里编码成了同一个值，**页面无法区分**。这是契约侧的缺口：修法是把那两个端点写成 `[]`（连同 §5.1 的补字段一起做），在那之前页面只能靠散文补回来。

### 5.5 漂移门禁（分级，不是一刀切）

v1 说「阻断会让每次改样式都因上游变动而红」是稻草人 —— 一个 `paths` 过滤的 required check 改 CSS 永远不会红。分级：

| 上游变化 | 处理 |
| --- | --- |
| 字段增减、`experimental → compatible` | 告警。站点少说能力，方向安全 |
| **`compatible → experimental` 降级** | **阻断**。站点会继续用 compatible 徽章展示已降级的端点，这是「说错」不是「说旧」 |
| **端点 id 消失** | **阻断**。`getStaticPaths` 生成的已发布 URL 直接变死链，而 `check:links` 只遍历构建产物，看不见上游删除 |
| `unsupported_request_fields` 变化 | 阻断。它的含义是「某字段现在会被拒」，旧文档说它可用，读者照着发就是 400 |

契约近期被改过 16 次，最近三次是 `7117fdc fix(deepseek)`、`d3dc81d fix(deepseek)`、`24e5973 fix(anthropic)` —— 全是字段级语义变更，正是最会「说得错」的那类。

---

## 6. 配置参考

v1 的配置分组是编的：`auth` section 不存在，`alert` 应为 `alerts`，且漏掉一半真实 section。`internal/config/default.yaml` 的真实顶层是：

```
server  tls  storage  admin  usage  gateway  retry  circuit_breaker
alerts  security  metrics  audit  model_catalog  providers  logging
```

单页覆盖全部 15 个，按「常改」与「后果大」排序。两处后果最大、v1 完全没有页面的：

- `usage.timezone` —— 决定预算日边界与夏令时行为（`user-guide.zh-CN.md:327`）
- `security.trust_proxy_headers` —— 决定 CIDR 授权是否失效（`operator-guide.md:70-76`）

以及必须写清的两条：非回环监听必须启用 TLS（`validateListener`，唯一逃生口 `-allow-insecure-public-listen` 且只覆盖 `server.gateway_listen`）；单写者单数据目录，容器编排必须恰好一个副本、`Recreate` 而非滚动更新。

**引用函数名，不引行号**。`config.go` 有 1220 行、132 个 yaml tag、近一年 24 次提交，`validateListener` 距文件尾只有 40 行，上方任何插入都会让整章行号失效，而网站仓库感知不到。

`tools/configdoc`（从结构体 tag + 校验函数导出 `config-reference.json`）**提到 P1**，不是打磨项：它是把配置页的失效模式从「说错」降级为「说旧」的唯一手段。

---

## 7. 内容迁移映射

| 站点页 | 来源 | 处理 |
| --- | --- | --- |
| 对象模型 | `user-guide.zh-CN.md` §1 + CLAUDE.md 链路 | 重写 + 一张 SVG（沿用 HeroGraphic 视觉语言） |
| 我拿到了一个 Key | **无现成来源** | 从零写：拿 Key → 发第一个 curl → 读懂响应 → 4xx 自救 |
| 我要装一套 | `user-guide.zh-CN.md` §2、§3.1 | 重排 |
| 安装与首次初始化 | `operator-guide.md` Clean install / 容器形态 | 译中 |
| TLS 与反向代理 | `docs/todo/tls-acme-plan.zh-CN.md` §4/§5 + operator-guide | 形态 A/B |
| config.yaml 参考 | `default.yaml` + operator-guide | §6 |
| 认证、请求头与错误 | **全仓无权威清单** | 错误码今天只在 `user-guide.zh-CN.md` 的排障表和 `operator-guide.md` 各存一份、彼此不同且都不完整（见 §0.2 的 `price_unavailable`）。必须从 `internal/gateway*` 现提：同一个 error code 可能对应多个状态码，清单要按「code × 状态码 × 触发条件」三列写，这是写作不是重排 |
| 五个核心端点 + 实验性一览 | 契约 | §5 |
| 示例集 | `user-guide.zh-CN.md` §4.2–§4.6 + `DeveloperPage.tsx` 的 curl 模板 | Python/Node/Go/Java/Anthropic SDK 需新写 |
| 预算、限流与用量核算 | `user-guide.zh-CN.md` §5、§6、§7、**§12.1** | §12.1 的价格版本/成本调整/价格建议/pricing quarantine 是独立子系统，v1 整个漏掉 |
| 运维手册 | `backup-restore.md`、operator-guide、**`user-guide.zh-CN.md` §8 Metrics**、`docs/runbooks/` | §8 的 `metrics token`/`rotate`/`revoke` 与非回环 mTLS 在 v1 里无家可归 |
| 兼容性矩阵 | `compatibility/README.md` + 契约 | 生成表 + 手写口径 |
| 变更日志与升级 | `CHANGELOG.md`、`SECURITY.md`、`SUPPORT.md` | v1 一个都没挂。pre-1.0 且 CLAUDE.md 要求说明哪些变更需重新初始化，「我该不该升」是高频问题 |

**必须显式回答、而契约里一条都没有的四件事**（第一次调用就会撞上）：

1. `/v1/models` —— SDK 初始化后第一个动作常是 `models.list()`，Halro 对它有专门提示（`internal/gatewayapi/handler.go` 的 `unimplementedHints`），但**它不在 20 条 manifest 里**。契约作为「唯一事实源」在结构上生不出这一页，必须手写。
2. **429 的六个成因**：Project RPM / TPM / 并发、Provider 或 Deployment 并发、Token Guard、上游限流。
3. **404 `model_not_found`（Route 不存在）与 403 `model_not_allowed`（Route 存在但 Project 未授权）的区分** —— 只存在于代码里（`internal/gateway/service.go:226`、`:249`），契约里没有。
4. `Retry-After` 与 `WWW-Authenticate` 响应头 —— 代码会返回，契约里没有。

**「某个模型支不支持工具调用」在文档里走不通**，要直说：Gateway 不暴露 `GET /v1/models`，读者不知道自己的别名解析到哪个 Provider Profile，`profile_coverage` 是按 profile 组织的。文档必须明写「这个问题只能问管理员」，否则读者会在树里绕。

**截图**：现有 4 张（`01-dashboard-overview`、`02-project-attribution`、`03-usage-details`、`04-token-guard-policy`）**一张都不覆盖** Credential/Provider/Deployment/Route/Key 主链路，v1 写的「4 张可复用」是错的。近 90 天有 148 个 commit 动了 `web/src`，截图腐化的表现是「照着文档点，找不到那个按钮」。取舍：**第一版尽量不用截图**，改用文字步骤 + 对象模型图；确实需要的在专用演示实例上拍，用固定 fixture 数据，发布前逐张人工确认无真实密钥/凭据/用量/主机名 —— 图片过不了任何 grep 门禁，是唯一必须靠人的环节。

---

## 8. 发布与 CI（前置，不是收尾）

这两件在 v1 里被推迟到「待确认」，实际是 P0 的前置阻塞 —— 没有它们，「站点可发布」没有可验收的含义，§5.5 与 §10 的门禁也没有执行者。

1. **建 `.github/workflows/`**：`oven-sh/setup-bun` 钉版本 → `bun install --frozen-lockfile` → `bun run build` → link checker + 契约断言。注意站点用 bun，主仓 `web/` 用 npm，两套心智并存。
2. **定发布路径**：承接被删掉的 Cloudflare Worker + Static Assets 形态，或换 Pages。
3. **定域名**（`/docs` 还是 `docs.` 子域）：它决定 `astro.config.mjs` 的 `site` 与发布环境的 `PUBLIC_SITE_URL`，而 `site` 缺失会让 canonical、og:image、sitemap 全部不生成（当前 `dist/index.html` 里 `canonical`/`og:image` 出现次数为 **0**）。文档站靠搜索引擎进人，这个不能等。

---

## 9. 分阶段

**P-1（前置，1–2 天）**：§4.1 的三件验证 + §8 的三件。任何内容页之前。

**P0（可上线）**：对象模型 / 我拿到了一个 Key / 我要装一套 / 认证请求头与错误 / 五个核心端点 / 实验性端点一览 / curl 示例集。中英同写。

**P1**：TLS 与反向代理 / config.yaml 参考 / 预算限流与用量核算 / 运维手册 / 兼容性矩阵 / 变更日志入口；SDK 示例补齐；`tools/configdoc`。

**P2**：`EndpointTable` 的余量打磨；英文侧补齐（若 P0/P1 有页只做了中文）。

> §5.1 的契约补字段**从 P2 提到 P1**：探路草稿显示只需覆盖 5 个 compatible 端点、150 条描述、约 2–3 人日，而它决定 API 参考页是「字段名清单」还是「真正的参考」。P0 仍可在补字段前上线——那时 API 页是「字段名清单 + 被拒字段 + 请求头 + 偏差 + 覆盖矩阵」，后四项本身有增量价值，前一项如实呈现为清单，不假装是完整参考。

---

## 10. 验收（机器断言优先）

v1 的「每次改动后人工核对随机 3 个 API 页」删除 —— 它要防的正是机器能判、人容易漏的差别，且问题命中 63% 的 coverage 条目，抽 3 页抽不出来。改为：

- `check:api` —— 四态划分互斥且覆盖；`null` 与 `[]` 区分；`provider_profiles` 与 `profile_coverage` 集合相等；`unsupported_request_fields` 零孤儿。
- `check:links` —— 站内链接与锚点可达。trailing slash 归一、CJK 锚点 `decodeURIComponent` + NFC、重名标题 `-1` 后缀。跳过 `publicDir` 里的非 HTML（`dist/articles/*.md` 是原样发布的裸文件）。
- **产物密钥扫描** —— 递归匹配 `gw_[A-Za-z0-9_-]{20,}`、`sk-[A-Za-z0-9]{20,}`、`(AKIA|ASIA)[0-9A-Z]{16}`、`AIza[0-9A-Za-z_-]{35}`、`-----BEGIN .* PRIVATE KEY-----`，命中即失败。与主仓 `web/scripts/check-artifacts.mjs` 同源，且更必要：Pagefind 会把正文再索引一份，一次误粘贴会同时出现在两处。
- 搜索：验收改成「⌘K 首次唤起后新增传输 ≤ N KB」，那才是用户感知的数字。
- 可访问性：`lang` 属性正确（英文页不能是 `zh-CN`）、`h1` 唯一、`aria-current`、两套主题对比度 ≥ 4.5:1，配 axe 或 pa11y。
- hreflang / `link rel=alternate` / sitemap —— v1 通篇未提。

---

## 11. 风险与取舍

| 风险 | 处理 |
| --- | --- |
| 一个人写不完 | 已砍到 12–15 页；15 个 experimental 端点合并一页；中英同写，做不到就只做中文 |
| 契约同步半年后停在上线当天 | 触发器放 Halro 侧的 CI（§5.2），降级与删除阻断（§5.5） |
| Starlight 切换 markdown 管线 | P-1 第一件事就是实测（§4.1） |
| 「整页搬」的散文副本无门禁 | 只搬 `backup-restore.md` 一处，其余重写。搬运的那份在页首注明来源与同步日期 |
| 文档与发版无绑定 | `docs/guides/releasing.md` 三节里没有一节提文档站，需要加一步 |
| 截图腐化 | 第一版尽量不用截图（§7） |
| 素材本身不完整 | 搬运前逐条对代码核实。`price_unavailable` 即是先例：文档记 409，代码里 503 与 409 各有触发路径 |

## 12. 评审留下的未决项

1. ~~§5.1 的契约补字段是否值得~~ **已解决**：探路草稿（脚本渲染 20 个 manifest 为纯文本）显示实际只需 5 个端点、150 条描述、2–3 人日，已提到 P1。草稿本身是一次性产物，未入库。
2. `profile_revision` 是否随结构变更 bump —— 契约变更，需按 `docs/compatibility/README.md` 口径审慎评审。
3. 发布路径与域名（§8）—— P-1 必须拍板，不能进 P0。
4. `docs/README.md`（仓库现有文档索引，13 个分组）在站点上线后是保留、削成开发者索引、还是指向站点。不定会变成两份并存的索引。

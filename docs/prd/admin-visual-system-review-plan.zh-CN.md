# Halro Admin Console 全面视觉评审与视觉系统建设方案

> 执行状态（2026-09-17 评审，2026-09-17 复核修正）：
>
> - **已完成**：Phase 0（基线冻结）、Phase 1（全面评审），以及由评审产出的一轮 P1/P2 缺陷修复
>   （12 项发现中 10 项关闭）。正式规范见 `docs/design-system/`，评审与验收证据见
>   `docs/review/260917-visual-system/`。
> - **未执行**：Phase 2（冻结 Foundation）、Phase 3（建立核心组件）、Phase 4（代表页面试点）、
>   Phase 5（全量迁移）。§9.3 的语义间距 Token 未实现；裸值基线仅由 699 降至 692；
>   §9.6 的断点收敛未发生；`web/src/styles.css` 自基线 2901 行仅变动至 2909 行，
>   共享组件样式表仍为 13 行。对应未关闭发现为 VS-05 与 VS-09。
> - 因此本文件是**进行中的方案**，不是历史档案。Phase 2–5 的条款仍然有效，未实现的规范
>   条款不得据"已验收"删除或降级。

- 状态：In progress — Phase 0–1 完成 + 一轮 P1/P2 修复已落地；Phase 2–5 未执行
- 日期：2026-09-17
- 基线分支：`main`
- 基线提交：`364db696`
- 适用范围：Halro Web Admin Console（认证前页面、全局壳层、11 个主路由及其 Modal / Drawer / 状态页）
- 规范定位：Apple Human Interface Guidelines 原则在 Web 管理控制台中的转译，不是 iOS/macOS 界面的像素级仿制
- 预期结果：完成可追溯的现状评审，并形成可实现、可测试、可版本化的 Halro Visual System

## 1. 结论

本次工作不能被定义为一次“统一颜色和圆角”的视觉润色。Halro 已经拥有 Primitive Token、Light / Dark Semantic Token、部分共享组件契约、响应式规则和自动化设计系统测试，但页面级样式、组件级样式和业务布局仍混合在一个大型样式文件中，间距、密度、断点和交互状态没有形成完整的系统治理。

评审与建设应沿以下路径进行：

```text
真实任务与页面清单
  → 现状截图与代码资产盘点
  → 按统一量表评审
  → 问题分级与根因归类
  → 冻结视觉原则和 Foundation
  → 建立组件与页面模式
  → 代表页面试点
  → 全量迁移
  → 浏览器、主题、语言、缩放与无障碍验收
  → 自动化门禁防止回退
```

最终视觉系统应让管理员在不同主题、窗口宽度、语言和权限下仍能稳定回答三件事：

1. 我现在处于哪里，当前上下文是什么；
2. 什么信息最重要，哪里需要处理；
3. 哪个操作安全可用，执行后发生了什么。

## 2. Apple 规范的采用方式

### 2.1 采用原则

本方案主要转译以下 Apple HIG 原则：

- **Purpose / Focus**：界面服务于任务，内容和主要操作优先，视觉装饰不抢占注意力。参见 [Design principles](https://developer.apple.com/design/human-interface-guidelines/design-principles)。
- **Hierarchy / Alignment / Adaptivity**：按阅读顺序安排重要内容，通过对齐建立可扫描结构，并在窗口、字号、语言变化时保持熟悉。参见 [Layout](https://developer.apple.com/design/human-interface-guidelines/layout)。
- **Legibility / Typographic hierarchy**：优先系统字体、少量字体家族、清晰字号与字重层级，并在字号变化时保留相对层级。参见 [Typography](https://developer.apple.com/design/human-interface-guidelines/typography)。
- **Semantic color**：颜色按用途而不是按色值命名；同一颜色不承担相互冲突的含义；Light、Dark 和增强对比上下文分别验证。参见 [Color](https://developer.apple.com/design/human-interface-guidelines/color) 与 [Dark Mode](https://developer.apple.com/design/human-interface-guidelines/dark-mode)。
- **Perceivable / Operable / Adaptable**：不能只用颜色传达状态，要支持键盘、缩放、减少动画、高对比和辅助技术。参见 [Accessibility](https://developer.apple.com/design/human-interface-guidelines/accessibility)。
- **Restrained branding**：品牌强调色只用于真正需要强调的主操作、选中状态和少数关键反馈，不把所有交互都染成品牌色。参见 [Branding](https://developer.apple.com/design/human-interface-guidelines/branding)。

### 2.2 不直接照搬的内容

- 不复制 iOS 导航结构、原生控件尺寸、macOS 工具栏或 Liquid Glass 外观；Halro 是跨平台 Web 管理控制台。
- 不依赖 Apple 平台专属 API、SF Symbols 或受限设计资源；图标必须使用项目自有、许可清晰且语义稳定的资产。
- 不把“像 Apple”理解为大量模糊、透明、超大圆角、悬浮胶囊和动画。Apple 原则的核心是清晰、克制、熟悉、适配和可访问。
- 不为了视觉统一改变 Halro 的权限、安全、费用、Provider 证据或只读语义。

## 3. 当前系统初始基线

以下是制定本方案时的代码级事实，不是最终视觉结论：

| 基线项 | 当前事实 | 评审意义 |
| --- | --- | --- |
| 页面范围 | 11 个懒加载主路由，另有 Login、Setup、404、MFA 强制设置等壳层状态 | 必须覆盖完整用户旅程，不能只检查 Dashboard |
| 全局样式 | `web/src/styles.css` 约 2,901 行 | 页面、组件和布局规则耦合，修改影响面难以直观判断 |
| 设计系统 | 已有 Primitive Token、Light / Dark Semantic Token、Resource Card / List 契约 | 应扩展现有系统，不建立第二套 Token 或第三方 UI 体系 |
| Token | 当前设计系统 CSS 约 233 个自定义属性声明 | 需要区分有效语义、兼容别名、重复概念和未消费 Token |
| 字体 | 系统无衬线 + 系统等宽；字号 Token 为 12 / 13 / 15 / 18 / 22 / 32px | 已有合理起点，但页面标题仍存在 42/46px 级别的独立响应式值 |
| 间距 | 已有 4px 基础间距阶梯；自动化测试记录 699 个手写间距/圆角值 | Token 存在不等于被统一使用，需要逐步降低手写值而非一次性重写 |
| 圆角 | Token 为 3 / 6 / 10px / pill；业务样式仍存在多个独立值 | 需要把圆角绑定到组件语义，而不是继续增加尺寸 |
| 主题 | 已实现服务端持久化的 Light / Dark，主题色键集合与对比度有测试 | 评审要覆盖两套主题全部状态，不能只验默认 Dark |
| 响应式 | 现有断点包含 580、640、720、760、820、1120、1520px 及 rem 变体 | 需要收敛为内容驱动的少量布局模式，减少局部补丁 |
| 无障碍 | 已有 skip link、focus-visible、reduced motion、forced colors 与部分语义测试 | 需要从“存在规则”升级到真实键盘、缩放、读屏任务验证 |
| 共享组件 | 已有 PageHeader、Modal、ConfirmButton、Field、Combobox、Tabs、Empty / Error / Loading 等 | 应形成正式组件状态契约和使用边界 |
| 生成资产 | `internal/webui/dist` 是 `web/` 构建产物并嵌入 Go 二进制 | 任何 UI 实现必须同步构建并验证 bundle 无漂移 |

### 3.1 初步风险假设

评审需要验证而不是预设以下风险：

1. 页面信息结构可能仍按后端资源和实现模块排列，而不是按管理员任务排列；
2. 12px 文本使用比例较高，可能让长时间阅读、中文界面和低分辨率显示器产生密度压力；
3. 品牌绿同时出现在操作、选中、在线和数据强调中，可能存在语义过载；
4. 大量局部间距和断点可能造成相同组件在不同页面节奏不一致；
5. 30–36px 的桌面控件在触屏或窄屏环境中可能缺少足够命中区域；
6. 表格、长 ID、代码和 JSON 是主要适配压力，不能依靠整页横向滚动解决；
7. 部分系统组件已经存在，但页面仍可能通过业务 class 重复实现相近外观；
8. 视觉层级、语义结构、键盘顺序和 API 数据语义可能不一致，不能只凭截图下结论。

## 4. 评审目标与非目标

### 4.1 目标

1. 建立页面、组件、状态、主题、权限、语言和断点的完整评审范围。
2. 用同一量表评估 UI 结构、视觉层级、字体、颜色、间距、控件、图表、响应式和无障碍。
3. 将发现追溯到信息架构、组件缺口、Token 缺口或页面局部实现，避免只修截图表面。
4. 冻结 Halro Visual System 的 Foundation、组件、模式和内容规范。
5. 给出按风险和复用价值排序的迁移计划，不一次性重写全部页面。
6. 建立自动化与人工结合的验收门禁，确保后续页面不会重新漂移。

### 4.2 非目标

- 本方案阶段不直接改写现有 UI，也不重做业务流程。
- 不引入 Material UI、Ant Design、Tailwind、Bootstrap 或另一套完整组件库。
- 不把 Halro 改造成面向消费者的营销产品；它仍是高信息密度的安全与治理控制台。
- 不以减少信息为名隐藏费用完整性、权限、失败原因、证据来源或安全后果。
- 不把截图“好看”当成完成；任务可达、语义正确、真实数据可读和状态可恢复同样是验收项。
- 不在本期增加用户自定义字体、圆角、密度、强调色或任意主题编辑器。

## 5. 评审范围

### 5.1 用户旅程

至少完整评审以下旅程：

1. 首次 Setup → 登录 → MFA 强制配置；
2. Dashboard 判断系统状态和待处理问题；
3. Provider → Deployment → Route 的创建、测试、编辑、禁用和错误恢复；
4. Project → Gateway Key → Developer 调用样例；
5. Token Guard / Policy 的配置、只读查看和拒绝反馈；
6. Usage 的筛选、图表、失败明细 Drawer 和长 JSON 阅读；
7. Run Governance 的概览、Work Unit / Run / Attempt 下钻、Outcome / Definition；
8. Operations 的 webhook、audit、alerts 和分页；
9. Settings 的语言、Appearance、时区、运行设置、账户与 Master Key Custody；
10. 只读管理员、会话失效、401 / 403 / 404、空数据、慢请求和局部失败。

### 5.2 页面清单

| 页面/区域 | 主任务 | 重点评审项 |
| --- | --- | --- |
| App / Layout / Sidebar / Topline | 定位、导航、全局上下文 | 导航密度、活动态、折叠策略、账户区、窄屏结构 |
| Login / Setup / Boot / 404 | 进入系统、恢复错误 | 首屏层级、表单宽度、状态反馈、键盘路径 |
| Dashboard | 判断健康度与优先级 | 指标层级、图表、异常队列、首屏信息量 |
| Providers / Deployments / Routes | 管理上游资源 | Resource 组件一致性、行操作、技术详情、状态语义 |
| Projects / Developer | 建立访问边界并完成接入 | 步骤结构、密钥安全、代码可读性、复制反馈 |
| Policies | 配置安全策略 | 风险提示、条件表达、破坏性操作、复杂表单 |
| Usage | 分析费用、用量与失败 | 筛选密度、图表、表格、Drawer、长内容适配 |
| Run Governance | 理解业务工作与结果 | 对象关系、父子层级、主从视图、复杂响应式 |
| Operations | 观察告警、审计和外发 | 状态优先级、时间线、事件密度、分页 |
| Settings | 管理个人和实例设置 | 信息架构、表单分组、作用域、保存反馈 |
| Modal / Drawer / Toast / Menu | 完成局部任务 | 焦点管理、Escape、滚动、危险操作、堆叠层级 |

### 5.3 状态覆盖

每个组件和页面必须覆盖：

- default、hover、active/pressed、focus-visible；
- selected/current、disabled、read-only、loading；
- success、warning、danger、info、unknown；
- empty、partial、stale、error、permission denied；
- 短文本、长文本、长 ID、超长数字、未知费用、零费用；
- Light / Dark；中文 / English；administrator / read_only。

## 6. 评审方法

### 6.1 Step A：冻结可复现基线

记录：

- commit SHA、构建方式、浏览器及版本、操作系统、设备像素比；
- 测试账户角色、主题、语言、时区和数据 fixture；
- 页面 URL、筛选参数和关键数据状态；
- 已知环境限制和无法取得的真实 Provider 证据。

评审使用隔离的合成数据，不修改 live `data/`，不为了截图改变真实账户、密钥、Ledger 或运行状态。

### 6.2 Step B：建立视觉证据集

每个主页面保存以下截图：

- 1440×900：常见桌面主视图；
- 1280×800：紧凑桌面；
- 1024×768：窄窗口/平板横向参考；
- 820×1180：壳层切换边界；
- 390×844：常见手机窄屏；
- 320×800：产品声明的最小页面宽度；
- 200% 浏览器缩放：等效窄视口与字号压力。

代表页面同时采集 Light / Dark、中文 / English。交互组件保存状态板：默认、hover、focus、disabled、loading、error、selected。

### 6.3 Step C：代码与视觉双向审计

视觉发现必须回溯到以下一类根因：

- `IA`：信息结构或任务顺序；
- `PATTERN`：页面模式缺失或使用错误；
- `COMPONENT`：共享组件契约缺失或漂移；
- `TOKEN`：Foundation 缺项、语义错误或未消费；
- `LOCAL`：单页实现问题；
- `CONTENT`：文案、术语、格式化或国际化；
- `A11Y`：语义、焦点、对比、缩放、读屏或输入方式；
- `DATA`：视觉表达与真实业务/API 语义不一致。

同一个问题不得在多个页面分别修补；优先修复最高层的共同根因。

### 6.4 Step D：真实任务走查

评审者不只浏览页面，还要按任务完成操作，记录：

- 首次发现入口的时间；
- 完成任务的步骤和上下文切换次数；
- 是否出现不确定、回退或误触；
- 错误后是否能恢复；
- 操作结果是否有唯一、持久且可理解的反馈；
- 键盘路径是否与视觉顺序一致。

## 7. 统一评分量表

总分 100。分数用于比较和排序，不能抵消 P0/P1 缺陷。

| 维度 | 权重 | 核心问题 |
| --- | ---: | --- |
| 信息架构与任务流 | 15 | 页面是否围绕管理员目标组织，主路径是否明确 |
| 视觉层级与结构 | 10 | 标题、摘要、内容、辅助信息和操作是否有稳定主次 |
| 字体与可读性 | 10 | 字号、字重、行高、行长、数字/代码字体是否合理 |
| 颜色与主题 | 10 | 语义是否稳定，Light/Dark 是否完整，是否只靠颜色表达 |
| 间距、网格与密度 | 10 | 节奏是否一致，对齐是否稳定，密度是否匹配任务 |
| 组件与交互状态 | 15 | 同类控件是否一致，完整状态和反馈是否存在 |
| 响应式与内容适配 | 15 | 不同窗口、缩放、文本长度和输入方式是否可用 |
| 无障碍与包容性 | 15 | 键盘、焦点、对比、语义、减少动画、强制色是否可靠 |

### 7.1 单项评分

- `0`：缺失或阻塞任务；
- `1`：严重不一致，依赖猜测或绕行；
- `2`：基本可用，但存在明显层级/一致性/适配问题；
- `3`：满足规范，少量局部瑕疵；
- `4`：清晰、稳定、可复用，并有验证证据。

### 7.2 问题等级

- `P0`：阻塞关键任务，或造成安全、权限、费用、数据含义的危险误解；
- `P1`：高频任务明显不可用、不可访问，或主要断点/主题系统性失败；
- `P2`：明显一致性、层级、密度或反馈问题，但存在可靠替代路径；
- `P3`：细节优化和低风险品质提升。

进入全量迁移前：P0 必须为 0，P1 必须有已批准处置。最终验收：P0/P1 为 0，每个主页面不低于 85/100，系统级 Foundation 与组件不低于 90/100。

## 8. 各维度评审清单

### 8.1 UI 与信息结构

- 页面是否只有一个明确主标题和一个主任务；
- 主要操作是否出现在任务开始或结束的自然位置；
- 低频配置是否抢占高频监控的首屏；
- 主对象、父子对象和证据对象是否在视觉上明确分组；
- 是否把内部 ID、watermark、原始 JSON 放在业务结论之前；
- Tabs、侧栏、筛选、面包屑和 Drawer 是否各自承担明确层级；
- 返回、取消、关闭和恢复是否始终可发现；
- 只读状态是否在全局说明，而不是只表现为一片 disabled 控件。

### 8.2 视觉层级

- 第一眼是否能识别页面标题、关键状态、主要操作和异常；
- 卡片是否因为边框、阴影、背景层叠过多而形成“容器套娃”；
- 同权指标是否真的同权，父指标和子指标是否有结构而非只靠邻近；
- 技术证据是否比业务结论更安静，但仍可访问；
- 品牌色、危险色、警告色是否只在应该抢注意力时出现；
- 空态、加载态和错误态是否保持页面骨架，避免布局跳动。

### 8.3 字体、字号与文本

- 正文、标签、说明、标题、数值和代码是否使用正确文本角色；
- 正文不低于 15px，辅助文本不低于 12px；12px 不承载长段正文或关键后果；
- 不使用 Thin / Light；正文 Regular，控件和小标题 Medium/Semibold，强强调最多 Bold；
- 中文与英文字体 fallback 的基线、字宽和字重是否一致；
- 行长建议为 45–75 个拉丁字符；中文说明以约 24–38 个汉字为常用上限；
- 连续大写、字距和等宽字体只用于短标签、代码、ID 或真正的机器值；
- 大数字是否保留单位、精度和来源，不用字号掩盖未知或不完整；
- 200% 缩放和长英文下不应遮挡主要操作或丢失关键文本。

### 8.4 颜色、主题与图表

- 颜色全部从语义 Token 获得，业务 CSS 不直接使用 Primitive 或新色值；
- 品牌色只表达 primary action、current/selected 或少数品牌强调；
- success、warning、danger、info、unknown 不能共享同一种视觉语义；
- 状态必须同时有文字和必要的图形/结构，不能只靠红绿；
- Light / Dark 不是简单反色；raised、overlay、separator 在两种主题下分别验证；
- 正文对比度至少 4.5:1，大文本和关键非文本边界至少 3:1；
- 图表系列除颜色外还使用线型、marker、直接标签或清晰图例；
- 图表 tooltip、网格、轴、缺口、unknown 和 selected 状态在两种主题下可辨识；
- 颜色在 sRGB 普通显示器上仍可区分，不把 P3 作为可读性前提。

### 8.5 间距、网格与密度

- 4px 为微网格，常用布局按 8px 节奏；只允许从 Token 取值；
- 页面边距、Section 间距、Panel padding、Field gap、Row height 分别拥有语义 Token；
- 同一层级的卡片、标题、字段和操作栏应共享对齐线；
- 空白用于表达关系，不用额外边框补偿错误分组；
- 密集列表与配置表单可不同，但同类对象不能逐页自定密度；
- 1px hairline 属于边界，不应作为任意布局间距扩散；
- 每次迁移都降低手写尺寸基线，禁止借迁移增加新的散落值。

### 8.6 响应式与适配

- 断点由内容失效点决定，不按设备品牌命名；
- 侧栏、主内容、主从视图、表格和 Drawer 分别定义收缩策略；
- 320px 下整页不得横向滚动；数据表/代码区可在明确容器内局部滚动；
- 不能只隐藏重要列；折叠后需以 label/value、详情区或卡片形式保留；
- 触屏/窄屏主要点击目标至少 44×44 CSS px；桌面紧凑控件不得低于 32px，并保持清晰焦点；
- hover 不能是发现信息或完成任务的唯一方式；
- 窄屏 Modal 转为全宽/近全屏，标题和关闭入口固定可见，内容区独立滚动；
- 长 ID、URL、模型名、金额、中文和英文都要验证换行、截断与复制能力；
- 输入法、软键盘、safe area 与浏览器工具栏不能遮挡主要操作。

### 8.7 交互、反馈与动效

- 一个动作只在一个位置反馈一次：字段错误留在字段，移除对象后的结果进入通知；
- 错误不得自动消失，成功/信息可短暂显示但结果状态本身应持久可见；
- destructive action 使用项目 Modal，不使用 `window.confirm`；
- Loading 说明正在等待什么；超过可感知阈值后显示稳定骨架或进度；
- 动效用于解释状态变化和空间关系，不用于装饰；
- 常规状态变化建议 90–180ms，Drawer/Modal 可 180–240ms；
- `prefers-reduced-motion` 下移除位移、缩放和循环动画，同时保留非运动反馈；
- 页面不得因动画、字体加载或异步数据产生明显布局跳动。

### 8.8 无障碍

- 从地址栏开始只用键盘完成所有主任务；
- 焦点顺序与视觉顺序一致，Modal / Menu / Combobox 正确管理焦点；
- 页面导航后主内容获得可理解的焦点位置；
- 每个图标按钮有 accessible name；状态变化使用适当的 live region；
- Heading、landmark、list、table、form、fieldset、dialog 语义与视觉结构一致；
- 400% 缩放下保持单列可完成，不依赖双向滚动；
- `forced-colors`、高对比、色觉差异和减少动画均有人工证据；
- 不用 placeholder 代替 label，不用 tooltip 承载必需信息。

## 9. Halro Visual System v0.1 目标规范

本节是评审开始前的工作基线。评审证据可以调整具体值，但不能破坏语义结构。

### 9.1 设计原则

1. **证据优先**：先展示管理员做决定需要的事实，再展示内部实现细节。
2. **一个明确主路径**：每个页面只突出一个当前主要动作；次要动作降级，危险动作隔离。
3. **层级可见**：父子、主从、摘要与证据使用结构表达，不依赖位置猜测。
4. **克制的品牌感**：Halro lime 是强调，不是背景噪声或通用状态色。
5. **密度服务任务**：高信息量不等于小字体和小命中区；通过分组、对齐和渐进披露控制密度。
6. **主题等价**：Light / Dark 的信息、状态、对比和层级等价，不允许功能性差异。
7. **可恢复**：取消、返回、重试、复制、刷新和错误恢复始终明确。
8. **系统先于页面**：新增视觉规则优先落在 Token、组件或模式层，页面只负责业务布局。

### 9.2 字体系统

字体家族沿用本地系统字体，不引入网络字体：

```css
--font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI",
  "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei",
  "Noto Sans CJK SC", "Source Han Sans SC", sans-serif;

--font-mono: "SFMono-Regular", Consolas, "Liberation Mono",
  "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC",
  "Source Han Sans SC", monospace;
```

文本角色与承载它的 Token。"Token" 列写 `--font-size-*` 表示该角色直接由字号阶梯承担，
不再单独造角色名——一个与阶梯值完全相同的别名是重复概念，不是角色：

| 角色 | Token | 值 | 用途 |
| --- | --- | --- | --- |
| Page title | `--type-page-title-size` / `-compact` | 32 / 28px，行高 1.08 | 每页唯一 H1 |
| Section title | `--type-section-title-size` | 22px | 一级内容分区 |
| Card title | `--type-card-title-size` | 18px | Panel / Drawer / 卡片标题 |
| Body | `--type-body-size` | 15px | 正文、说明、表单值 |
| Label | `--type-label-size` | 13px | 控件标签、列表事实值 |
| Caption | `--type-caption-size` | 12px | 短辅助信息、时间、元数据 |
| Data / code | `--font-size-xs`–`sm` + `--font-mono` | 12–13px | ID、代码、端点、精确数值 |
| Metric | `--type-metric-size` | clamp(20px, 2vw, 30px) | 关键聚合值，不用于普通字段 |
| Display | `--type-display-size` | 34px | 仅入口屏（Login / Setup）表单标题 |
| Hero | `--type-hero-size` | clamp(34px, 4.2vw, 46px) | 仅入口屏主标题 |

约束：

- 12px 是绝对下限，不把正文压到下限；
- **控制台页面**标题取消 42/46px 的营销式放大，强调层级而不是体量；
- Display / Hero 是入口屏的显式例外，不是通用标题角色。任何控制台页面使用它们都是缺陷。
  这两个角色存在的理由是把例外命名一次，而不是让两个艺术化字号散落在业务 CSS 里；
- 中文标签默认使用 Sans，不为了技术感强制 Mono；
- ID、路径、代码、时间戳和等宽数字才使用 Mono；
- 字号不单独表达层级，需结合字重、颜色、间距和位置。

未实现项（不是已完成，也不是已放弃）：本表不再为角色定义独立的窄屏字号。窄屏只对
Page title 生效（32 → 28px）。Section title / Label 的窄屏档位在有证据证明必要前不实现，
避免造出无消费点的 Token。

### 9.3 间距系统

保留当前 4px 基础阶梯：

```text
0, 4, 8, 12, 16, 20, 24, 32, 40, 48, 64
```

本表原为一份未经验证的提案，2026-09-17 复核时按"每个 Token 必须有真实消费点"重新裁决。
一个没有调用点的语义别名不会减少裸值，只会增加一个必须维护的名字：

| 语义 Token | 值 | 裁决 |
| --- | ---: | --- |
| `--layout-page-inline` | clamp(16px, 4vw, 64px) | **已实现**，消费于 `.main`。原为裸 `4vw`，在 320px 下只有 12.8px，在宽屏下超过 100px |
| `--layout-page-block-end` | 72px | **已实现**，消费于 `.main`。值取自实现而非提案的 64px，避免制造未经评审的视觉变化 |
| `--component-row-padding` | — | **不新增**。已由 `--resource-row-padding-block` / `-inline` 承担，再造一个是重复概念 |
| `--layout-section-gap` | — | **暂不实现**。现状是 `.section-heading` 的 22px / 24px 两个不同值，统一到单一 Token 是视觉变更，需证据支持 |
| `--layout-panel-gap` | — | **不新增**。直接使用 `--space-4` |
| `--component-panel-padding` | — | **不新增**。直接使用 `--space-5` / `--space-6` |
| `--component-field-gap` | — | **不新增**。直接使用 `--space-2` |
| `--component-action-gap` | — | **不新增**。直接使用 `--space-2` |

规则：

- 页面只能决定网格列、内容顺序和响应式折叠；
- 组件内部节奏默认直接取 `--space-*` 阶梯。**只有当一个值必须在多个组件间同步变化时**，
  它才升格为语义 Token；否则别名只是给阶梯值换了个名字；
- 新增任何 `--color-* / --type-* / --layout-* / --control-*` 角色必须同时有调用点，
  由 `design-system.test.ts` 的 `declares no semantic role the product never reads` 精确断言。

减少裸值的真实度量不是本表的长度，而是棘轮基线（当前 691），它仍由 VS-05 跟踪。

### 9.4 圆角、边界、阴影与层级

先收敛现有值，不追求“更圆”：

| 层级 | 圆角 | 表达 |
| --- | ---: | --- |
| Small | 3–4px | 小标签、代码块内部元素、紧凑标记 |
| Control | 6px | Button、Input、Select、Tab |
| Surface | 10px | Card、Panel、Popover |
| Overlay | 12px | Modal、Drawer、Menu 容器 |
| Pill | 999px | 仅 Badge、状态胶囊、单行筛选 Chip |

- 普通 Surface 依靠背景层级和一条边界，不同时叠加高对比边框、重阴影和渐变。
- Elevation 只定义 `flat / raised / overlay` 三层。
- 同一页面最多使用三个表面层级；嵌套 Panel 优先使用 divider 或 section spacing。
- 模糊只用于确实需要与滚动内容建立空间关系的壳层/overlay；不作为装饰默认值。

### 9.5 颜色系统

沿用 Primitive → Semantic → Component 的单向依赖：

```text
Primitive palette
  → Semantic roles (canvas / surface / text / border / action / status / data)
  → Component tokens (button / input / nav / badge / chart / overlay)
  → Page layout
```

必须具备的语义组（2026-09-17 复核后的裁决状态。此清单是规范，
`design-system.test.ts` 的 `declares the specified semantic color roles` 按它断言，
而不是按已声明的 Token 反写）：

| 语义组 | 状态 |
| --- | --- |
| `canvas / surface-default / surface-subtle / surface-raised / overlay` | 已实现 |
| `text-primary / secondary / tertiary / inverse` | 已实现 |
| `border-default / strong`、`focus-ring` | 已实现 |
| `action-primary / hover / foreground / secondary` | 已实现 |
| `status-success / warning / danger / info` 的 text/surface/border/icon | 已实现 |
| `status-neutral` 的 text/surface/border/icon | 已实现。承载"管理员主动关闭"，此前由 `--color-text-tertiary` + `--line-strong` + `--overlay-hover` 三个不同族拼装 |
| `chart-series-1/2 / series-1-fill / grid / axis / tooltip` | 已实现，经 `TrendChart` 读取 |
| `scrim / shadow-base` | 已实现 |
| `status-unknown` 的 text/surface/border/icon | 已实现（VS-13）。`partial` 保留 warning，`unknown` 用虚线边界 + 无填充；与 `neutral` 的区分是结构而非色相，因为状态本就不允许只靠颜色表达 |
| `action-disabled` + `-border` | 已实现（VS-14）。取代 4 处 `opacity`——透明度让对比度取决于背景，同一写法曾散落在 1.90:1 到 6.93:1 之间 |
| `text-link` | 已实现（VS-15）。颜色 + 下划线，`.text-link` 用于正文链接；全局 `a` 规则保持不变，导航与按钮型锚点依赖继承 |
| `action-pressed` | **不实现**。全站 `:active` 只用位移表达按压，无颜色调用点 |
| `chart-selection / chart-unknown` | **不实现**。图表无选中态与 unknown 序列，无调用点 |

禁止：

- 页面按 Light/Dark 写分支；
- 业务 CSS 直接使用 `--p-*`；
- 用 brand lime 表达 success；
- 用 danger 红表达“管理员主动禁用”的中性状态；
- 仅靠透明度表示 disabled，导致文本或边界不可读。`opacity` 的对比度是背景的函数而不是规则的
  属性，因此 disabled 必须命名前景色，由对比度断言持有。唯一例外是原生复选框：它不承载文本，
  且 UA 自己绘制 disabled 外观。

### 9.6 响应式布局模式

目标断点不是替换所有现有值的机械常量，而是收敛后的布局模式。模式名称与范围的唯一权威是
`docs/design-system/foundations.md`；下表与其保持一致，实现以 `web/src/styles.css` 的壳层
断点（1120 / 820 / 580）为准：

| 模式 | 范围 | 行为 |
| --- | --- | --- |
| Wide | `> 1120px` | 固定侧栏；多列 Dashboard；主从视图可并排 |
| Regular | `821–1120px` | 保留侧栏；两列减少；表单与详情降低列数 |
| Compact | `581–820px` | 侧栏切换为顶部 disclosure 导航；主内容单列；交互目标 44px |
| Small | `320–580px` | 单列；28px 标题；Modal 近全屏；卡片或局部滚动替代表格主视图 |

现有 640、720、760、1520、70rem、64rem 等阈值只能作为组件内容断点存在，不得再创造新的全局
壳层模式，也不得为单个文案长度新增全局断点。

### 9.7 组件目录

视觉系统必须为下列组件定义 Anatomy、Variants、Size、State、Content、Keyboard、Responsive 和 Do/Don't：

| 类别 | 组件 |
| --- | --- |
| Actions | Button、IconButton、ButtonGroup、OverflowMenu、CopyAction |
| Navigation | SidebarItem、Topline、Tabs、SegmentedControl、Breadcrumb/BackLink |
| Inputs | Field、Input、Textarea、Select、Combobox、Checkbox、Radio、Switch、SearchField、Date/Time field |
| Feedback | InlineMessage、Notice、Notification、Progress、Skeleton、Loading、ErrorState、EmptyState |
| Status | Badge、StatusDot+Label、HealthSummary、EvidenceMark |
| Containers | Section、Panel、Card、Toolbar、Divider、Disclosure |
| Data display | Metric、DescriptionList、ResourceRow、ResourceCard、Table、Timeline、CodeBlock、Chart |
| Overlays | Modal、Drawer、Popover、Menu、Tooltip、ConfirmDialog |
| Shell | PageHeader、PageActions、FilterBar、Sidebar、AccountMenu |

每个可交互组件至少定义：

```text
default → hover → pressed → focus-visible → selected/current
        → disabled → loading → error → read-only
```

### 9.8 页面模式

页面不自行组合任意组件，而使用以下模式：

- `Overview`：PageHeader → decision metrics → attention queue → trends/details；
- `Resource list`：PageHeader → toolbar/filter → ResourceRow/Card → pagination/empty；
- `Master-detail`：filter/list → selected detail Drawer/side panel → nested evidence；
- `Settings`：scope summary → grouped sections → localized save feedback；
- `Workbench`：step/context → editor/request → result/evidence；
- `Audit/Timeline`：filter → event sequence → detail；
- `First run`：why → prerequisite → ordered steps → verification → next action。

### 9.9 图标与数据可视化

- 图标采用统一 24×24 viewBox、1.5–2px stroke、round cap/join；同一语义只使用一个图形。
- 图标不能代替文本表达陌生业务概念；Provider、模型能力、费用证据优先文字。
- 图标按钮有 tooltip 但 tooltip 不是 accessible name 或必要说明的唯一来源。
- 图表的目标是回答问题，不以装饰面积评价；必须写清时间范围、时区、单位、缺口和数据完整性。
- 默认最多突出 1–2 个序列；更多序列使用可选择图例、分面或表格，不制造彩虹图。

### 9.10 内容与国际化

- 按“对象 + 状态/动作”命名，避免含糊的“确定”“提交”“配置”；
- Button 使用动词，页面标题使用名词，危险确认写明对象和后果；
- 中文与英文维护相同信息结构，但允许自然表达，不追求逐字等长；
- 时间、日期、数字、货币、百分比和时区统一使用格式化层；
- `unknown`、`estimated`、`free`、`US$0.00` 必须保持不同含义；
- 文案不承诺 Provider 能力、生产状态或费用事实，除非数据来源能够证明。

## 10. 交付物

评审与建设最终必须产出以下仓库内资产：

1. `review-plan.md`：冻结范围、角色、方法、环境和时间表；
2. `page-inventory.md`：页面、任务、状态、角色、主题、断点覆盖矩阵；
3. `visual-baseline/`：带 URL/fixture/viewport 元数据的截图索引；
4. `findings.md`：每项含证据、根因、等级、范围、建议和验收方式；
5. `scorecard.md`：页面与系统维度评分，不隐藏阻塞问题；
6. `visual-principles.md`：经评审批准的视觉原则和反例；
7. `foundations.md`：Typography、Color、Spacing、Grid、Radius、Elevation、Motion、Icon；
8. `components.md`：组件 anatomy、variants、states、keyboard、responsive；
9. `patterns.md`：Overview、Resource list、Master-detail、Settings 等页面模式；
10. `migration-map.md`：旧 class/组件 → 新 Token/组件/模式映射；
11. `acceptance-report.md`：最终浏览器、主题、语言、权限、缩放和自动化证据；
12. 源码内 Token / 组件实现、测试，以及同步生成的 `internal/webui/dist`。

建议执行目录：

```text
docs/review/YYMMDD-visual-system/
  review-plan.md
  page-inventory.md
  findings.md
  scorecard.md
  visual-baseline/
  acceptance-report.md

docs/design-system/
  visual-principles.md
  foundations.md
  components.md
  patterns.md
  migration-map.md
```

本文件是总方案；日期化评审目录记录一次候选基线的证据，`docs/design-system/` 则是持续维护的正式规范。

## 11. 分阶段执行计划

### Phase 0：基线冻结与工具准备

工作：

- 冻结 SHA、浏览器、fixture、账户角色和截图矩阵；
- 建立页面/状态/主题/语言/断点清单；
- 记录现有 Token、组件、裸值、断点和 class 分布；
- 确认本地实例与合成数据不触碰 live 数据；
- 定义截图命名和发现记录模板。

完成标准：任一评审者可复现同一页面和数据状态；未开始视觉修改。

### Phase 1：全面评审

工作：

- 按真实用户旅程完成代码 + 浏览器双向审计；
- 对页面、壳层、共享组件和 Foundation 分别评分；
- 每个发现归类到 IA / Pattern / Component / Token / Local / Content / A11Y / Data；
- 形成 P0–P3 清单、截图和最小复现；
- 单独记录“视觉问题背后的业务语义问题”。

完成标准：所有主页面和关键状态都有证据；没有“看起来不统一”这种不可执行发现。

### Phase 2：冻结 Foundation

工作：

- 裁决字体角色、颜色角色、间距、圆角、阴影、动效和图标；
- 合并重复 Token，增加缺失的语义 Token；
- 为 Light / Dark 同时定义值；
- 扩展 contrast、Token parity、裸值和字号门禁；
- 建立只用于开发/测试的视觉样本页或组件 fixture。

完成标准：Foundation 文档、CSS 和测试一致；业务页面不能绕开系统新增颜色或尺寸。

### Phase 3：建立核心组件

优先顺序：

1. Button / IconButton / Field / Select / Combobox；
2. PageHeader / Section / Panel / Toolbar；
3. Badge / Notice / Empty / Error / Loading；
4. ResourceRow / ResourceCard / Table / DescriptionList；
5. Tabs / SegmentedControl / Menu；
6. Modal / Drawer / ConfirmDialog / Notification；
7. Metric / Chart / Timeline / CodeBlock。

完成标准：每个组件有状态矩阵、键盘契约、Light/Dark、窄屏和减少动画证据。

### Phase 4：代表页面试点

选择四个结构差异最大的页面：

- Dashboard：指标、图表和注意力层级；
- Providers：资源列表、测试状态和行操作；
- Run Governance：复杂对象关系和主从布局；
- Settings：表单、作用域和保存反馈。

试点目标是验证系统能覆盖真实复杂度，而不是单独把四页做漂亮。

完成标准：不新增单页专用视觉概念；组件和模式经试点修订后稳定。

### Phase 5：全量迁移

建议顺序：

1. 全局 Layout、Login、Setup、通用状态；
2. Deployments、Routes、Projects 等资源页；
3. Usage、Operations、Developer、Policies；
4. 所有 Modal、Drawer、Menu、错误和空态；
5. 清理旧 class、兼容别名和无消费 Token。

每次迁移只改一个可审阅范围，保存 before/after 证据并降低相关裸值基线。

### Phase 6：系统验收与治理

工作：

- 全页面主题、语言、权限、断点、缩放和键盘复验；
- 检查视觉回归、bundle、性能和生成资产；
- 完成接受/剩余风险报告；
- 将设计系统规则写入贡献指南和 PR checklist；
- 设定后续 Token/组件变更的 owner 与评审方式。

完成标准：Definition of Done 全部满足，且不存在未裁决 P0/P1。

## 12. 验证策略

### 12.1 迭代期间

- CSS、Token、字体、间距、主题改动：运行 `npx vitest run src/design-system.test.ts`，并做受影响页面的 Light/Dark 视觉检查；
- 行为、语义或组件逻辑改动：先运行直接受影响测试文件；
- TypeScript 类型或组件逻辑改动：运行 typecheck；
- 响应式改动：至少验证触发该规则前后两个宽度，不只验证一个截图；
- 不在每次 CSS 调整后运行完整前端套件。

### 12.2 页面验收矩阵

每个主页面至少完成：

```text
Light × Dark
Chinese × English
administrator × read_only（适用时）
1440 × 1024 × 820 × 390 × 320
100% × 200% × 400% zoom（代表页）
mouse × keyboard
normal × empty × loading × partial/error
```

不要求对所有排列做笛卡尔积截图，但必须用风险矩阵覆盖每一种变量及其关键组合。

### 12.3 最终前端门禁

在最终候选 push 前只运行一次完整门禁：

```bash
cd web
npm run typecheck
npm test
npm run build
cd ..
git diff --exit-code -- internal/webui/dist
git diff --check
```

还要执行：

- 浏览器控制台无新增错误；
- 页面级横向溢出检查；
- 键盘主旅程；
- Light/Dark 截图回归；
- 对比度、forced-colors、reduced-motion；
- 构建产物与源代码同步检查。

纯 Markdown 文档追加不重复触发已经成功且代码未变的完整门禁。

## 13. 视觉回归与自动化治理

### 13.1 必须自动化的内容

- Light/Dark Semantic Token 键集合一致；
- 关键文本与边界对比度下限；
- 业务 CSS 禁止新色值和 Primitive Token；
- 字号下限与字体角色 Token；
- 手写 spacing/radius 数量只能降低或由明确评审更新；
- 关键组件的 DOM 结构、ARIA 和状态类；
- 中文/英文 locale key 对等；
- 构建 bundle 无漂移。

### 13.2 不适合只靠自动化的内容

- 视觉注意力是否正确；
- 主次关系是否一眼可见；
- 卡片嵌套是否造成噪声；
- 中英文换行是否自然；
- 图表是否真正回答管理员问题；
- 触屏和键盘操作是否舒适；
- 复杂页面在真实数据下是否仍可扫描。

这些项目必须保留人工浏览器证据和评审裁决。

## 14. Definition of Done

只有同时满足以下条件，才能称为“形成一套视觉系统”：

### 14.1 规范完成

- Foundation、Components、Patterns、Content 四层文档齐全；
- 所有 Token 有语义、使用范围和 Light/Dark 值；
- 所有核心组件有状态、键盘、响应式和内容契约；
- 页面迁移映射和废弃规则明确。

### 14.2 产品完成

- 所有主页面使用同一套视觉层级和页面模式；
- 中文/英文、Light/Dark、管理员/只读状态等价；
- 320px 到宽屏可完成主任务，200%/400% 缩放不丢失关键能力；
- 数据表、长 ID、代码和 JSON 有明确的适配与复制策略；
- destructive、错误、partial、unknown 等高风险状态不依赖颜色或瞬时 Toast。

### 14.3 工程完成

- 页面不直接新增颜色 Primitive、裸色值或主题分支；
- 裸 spacing/radius 基线显著下降，并由测试继续收紧；
- 组件重复样式迁移到设计系统层；
- focused tests、typecheck、全量测试、production build 和 bundle drift gate 通过；
- `web/src` 与 `internal/webui/dist` 在同一实现提交中交付。

### 14.4 证据完成

- 每个 P0/P1 有关闭证据；P2/P3 有接受、延期或关闭裁决；
- 代表页面有 before/after、viewport、主题、语言和数据状态元数据；
- 最终报告明确区分代码级完成、浏览器验收和任何尚未完成的真实环境验收；
- 不把单元测试、构建成功或一张截图表述为全系统视觉验收。

## 15. 决策边界与裁决结果

以下问题原定在 Phase 1 证据完成后裁决。2026-09-17 复核时发现其中四项从未留下裁决记录，
现一并裁定。"不做"是对 v0.1 的裁定，不是永久否决；每项写明重新打开的条件：

| # | 问题 | 裁决 | 理由与重开条件 |
| --- | --- | --- | --- |
| 1 | 是否增加 `System` Appearance | **v0.1 不做** | Appearance 是服务端持久化的账户偏好，`System` 需要客户端媒体查询在每次加载时覆盖服务端状态，两个真相来源会让"主题等价"无法断言。重开条件：Appearance 改为客户端优先，或明确定义服务端值与系统值的优先级 |
| 2 | 是否需要 Comfortable / Compact 密度 | **v0.1 不做** | 与 `visual-principles.md` 第 3 条"默认保持单一可靠密度"一致。两档密度会让每个组件的状态矩阵翻倍，而当前单档密度尚未在全部页面稳定 |
| 3 | 820px 以下侧栏形态 | **已裁决：顶部 disclosure** | 见 VS-01。带 `aria-expanded` / `aria-controls`，Enter 打开、Tab 首先到达 Overview，11 个目的地与账户动作全部可达 |
| 4 | Small 模式下表格形态 | **已裁决：两者并存，按列语义分** | 见 `patterns.md` Data table：列语义允许时转为带 `data-label` 的卡片行；高密度技术表保持容器内局部横向滚动。禁止由 body 承担横向滚动 |
| 5 | 是否建立独立组件预览页 | **不做** | 它需要路由、权限判定和导航入口，必然成为第二套产品导航；而它要提供的证据已由 `design-system.test.ts` 的状态/对比度/角色断言与组件测试承担。重开条件：出现自动化无法表达的组件状态证据需求 |
| 6 | 是否增加高对比独立主题 | **v0.1 不做** | 第三套主题会把"Light / Dark 键集合一致"的门禁变成三方比对，而当前 `forced-colors: active` 与 4.5:1 / 3:1 对比度断言已覆盖增强对比场景。重开条件：真实用户或合规要求证明 forced-colors 不足 |
| 7 | 是否支持 RTL | **超出范围** | 产品当前只有 zh-CN 与 en-US，均为 LTR。为不存在的 locale 写 logical property 无法验证。重开条件：产品增加任一 RTL locale——届时 RTL 是该 locale 交付的一部分，不是补丁 |

## 16. 首轮执行建议

第一轮只完成 Phase 0–1，不直接大规模改 CSS：

1. 建立日期化评审目录和页面覆盖矩阵；
2. 准备隔离 fixture，采集 Dashboard、Providers、Usage、Run Governance、Settings 五个代表页面；
3. 完成 100 分评分与 P0–P3 发现；
4. 把发现归并为 Foundation、Component、Pattern 和 Local 四类整改包；
5. 提交评审报告供裁决；
6. 只有视觉原则、目标 Token 和试点页面得到批准后，再进入实现。

这能把“审美偏好”转化为有证据的设计决策，也能避免在尚未理解全局问题时直接重写 2,901 行业务样式。

## 17. 参考资料

- [Apple Human Interface Guidelines](https://developer.apple.com/design/human-interface-guidelines)
- [Apple HIG — Design principles](https://developer.apple.com/design/human-interface-guidelines/design-principles)
- [Apple HIG — Layout](https://developer.apple.com/design/human-interface-guidelines/layout)
- [Apple HIG — Typography](https://developer.apple.com/design/human-interface-guidelines/typography)
- [Apple HIG — Color](https://developer.apple.com/design/human-interface-guidelines/color)
- [Apple HIG — Dark Mode](https://developer.apple.com/design/human-interface-guidelines/dark-mode)
- [Apple HIG — Accessibility](https://developer.apple.com/design/human-interface-guidelines/accessibility)
- [Apple HIG — Branding](https://developer.apple.com/design/human-interface-guidelines/branding)
- [WCAG 2.2](https://www.w3.org/TR/WCAG22/)
- `web/src/design-system/README.md`
- `docs/prd/prd-admin-design-system-appearance.zh-CN.md`
- `docs/milestones/evidence/admin-appearance-qa-2026-08-04.md`
- `docs/prd/run-governance-admin-ui-redesign-plan.zh-CN.md`

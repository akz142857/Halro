# Halro Admin Console 设计系统升级与 Appearance（Light / Dark）PRD

- 状态：已实现并完成代码级验收
- 目标版本：v1.0.0
- 日期：2026-08-04
- 文档语言：中文
- 适用范围：Halro Web Admin Console

## 1. 背景

Halro Admin Console 已形成明确的深色安全运维视觉风格，并具备响应式布局、中英文国际化、键盘焦点、语义化状态和一批可复用 React 组件。当前样式仍主要集中在单一 `web/src/styles.css` 中：虽然已经使用 `--bg`、`--surface`、`--text`、`--muted`、`--lime` 等基础变量，但组件样式仍混有大量硬编码十六进制色与 `rgba()`，颜色、间距、圆角、阴影、字号、层级和交互状态尚未形成完整、可治理的设计系统。

现有页面通过 `:root { color-scheme: dark; }` 固定为 Dark。随着 Settings、MFA、Master Key Custody、用量分析和治理页面持续扩展，直接为每个组件追加 Light 覆盖会造成以下问题：

- 同一语义在不同页面使用不同颜色或间距；
- Light 模式容易出现遗漏、对比度不足、图表不可辨识和状态仅靠颜色表达；
- 新组件缺少统一的状态、尺寸和无障碍契约；
- 全局 CSS 修改影响面不清，回归验证成本持续增加；
- 主题能力与管理员偏好、服务端持久化及无浏览器持久化安全边界尚无产品契约。

因此，本期先建立 Admin Console 设计系统并迁移现有界面，再在该系统上交付 Appearance 的 Light 与 Dark 模式。Appearance 不是一组页面级颜色补丁，必须由同一套语义 Token 和组件契约驱动。

## 2. 目标

1. 建立可版本化、可测试、可文档化的 Halro Admin Console 设计系统。
2. 将现有 Dark 视觉迁移到语义化 Token，保持品牌识别和主要交互行为不变。
3. 为已登录管理员提供 `Light` 与 `Dark` 两种 Appearance，并在整个 Admin Console 一致生效。
4. 将 Appearance 作为管理员个人偏好持久化到服务端，不写入浏览器持久化存储。
5. 主题切换即时生效，保存失败时可恢复到服务端已确认值，并向用户明确反馈。
6. 确保两种主题满足键盘操作、缩放、减少动画、高对比可辨识和 WCAG 2.2 AA 级对比度要求。
7. 为后续新增页面提供明确的 Token、基础组件、使用规范和质量门禁。

## 3. 非目标

第一版不包含：

- 跟随操作系统的 `System` / `Auto` Appearance；
- 自定义主题、品牌换肤、租户配色或用户自选强调色；
- 高对比度独立主题；
- 字体大小、密度、圆角或动效的用户自定义；
- 移动端原生应用、CLI、文档站或第三方嵌入页面的主题；
- 引入 Material UI、Ant Design、Tailwind 等完整第三方组件体系；
- 借主题改造重新设计所有信息架构或业务流程；
- 将主题偏好写入 Local Storage、Session Storage、IndexedDB、URL 或可被前端脚本长期读取的自定义 Cookie。

`prefers-reduced-motion`、`forced-colors` 等系统无障碍信号仍必须被尊重，但它们不等同于新增 `System` Appearance。

## 4. 核心产品决策

### 4.1 先完成语义设计系统，再开放双主题

主题必须通过语义 Token 切换。业务页面和组件不得根据主题名称写分支，也不得使用 `.light .some-page` 形式逐页修补。

实现顺序冻结为：

```text
视觉资产盘点
  → Primitive Token
  → Semantic Token
  → Component Token / 基础组件契约
  → 现有 Dark 迁移与视觉回归
  → Light Token
  → Appearance 设置与持久化
  → 全页面双主题验收
```

### 4.2 第一版只提供 Light 与 Dark

- `Dark`：保留当前深色、安全运维风格，作为升级后的默认模式；
- `Light`：提供低眩光、清晰层级的浅色界面，不简单反转现有颜色；
- 不展示不可用的 System 选项；
- 未来增加 System 时必须另行定义解析优先级、浏览器监听和登录前主题策略。

### 4.3 Appearance 是管理员个人偏好

Appearance 只影响当前管理员看到的界面，不改变实例运行、安全策略、其他管理员或 Gateway 数据面。

- 未保存过 Appearance 的既有管理员迁移为 `dark`；
- 新管理员默认值为 `dark`；
- 已登录后使用当前管理员服务端偏好；
- 登录、首次初始化、Session 检查和未认证错误页第一版继续使用实例默认 `dark`；
- 登出后立即移除个人主题状态并恢复未认证默认 `dark`；
- 不根据浏览器或操作系统偏好隐式改变主题。

这样可保持升级视觉兼容，并避免在认证前暴露或猜测某个管理员的个人偏好。

### 4.4 服务端是偏好真相源

复用现有 revision / `ETag` / `If-Match` 管理员偏好契约，在同一偏好资源中增加 Appearance。

```json
{
  "locale": "system",
  "appearance": "dark",
  "revision": 4
}
```

- `appearance` 只接受 `light | dark`；
- `PUT /admin/api/v1/preferences` 必须提交完整可写偏好对象，避免更新语言时覆盖 Appearance，反之亦然；
- revision 冲突返回既有冲突语义，客户端重新获取真相源后提示用户；
- 偏好变更写入既有 `admin.preferences.update` Audit，审计记录字段名和新值，不记录设备、浏览器指纹或系统主题；
- Appearance 不属于秘密，但仍不得为了首屏主题绕过现有无浏览器持久化边界。

### 4.5 即时预览，成功后确认

用户在 Appearance 控件中选择 Light 或 Dark 后：

1. 当前页面立即应用所选主题；
2. 控件进入保存中状态并调用偏好 API；
3. 成功后更新本地已确认基线，并以非打断式状态文案确认；
4. 失败时恢复到上一个服务端已确认主题，保留可理解的错误提示和重试入口；
5. 保存期间重复选择以最后一次明确选择为准，客户端不得产生乱序结果覆盖。

主题切换不刷新页面，不丢失表单内容、滚动位置、焦点位置、查询缓存或未完成操作。

## 5. 用户场景与流程

### 5.1 修改 Appearance

1. 管理员进入“设置 → 通用”。
2. 在“外观 / Appearance”卡片中看到 Light 与 Dark 两个带视觉预览的单选项。
3. 当前已生效项同时具有选中状态、文本标签和 `aria-checked`，不能只靠边框颜色表达。
4. 管理员选择另一模式，整个 Console 立即切换。
5. 保存成功后显示“外观设置已保存”；失败则恢复并显示错误。

### 5.2 跨会话恢复

1. 管理员保存 Light 后退出登录。
2. 登录页恢复未认证默认 Dark。
3. 管理员再次登录，Session 与偏好加载成功后应用 Light。
4. 同一账号在另一浏览器或设备登录时，使用同一服务端偏好。

### 5.3 多标签页行为

- 第一版不通过浏览器存储事件或 Broadcast Channel 主动同步其他标签页；
- 其他标签页在重新导航、重新获取偏好或刷新后获得最新值；
- 并发保存遵循 revision 冲突，不允许最后响应静默覆盖较新的服务端偏好。

### 5.4 加载与失败降级

- 初始 HTML 和应用启动状态使用 Dark，保证任何时刻都有完整可读样式；
- 偏好尚未返回时不显示无样式内容；
- 偏好请求失败时保持 Dark 或当前已确认主题，Console 其他功能仍可使用；
- 无效或未知 Appearance 值一律规范化为 Dark，并在服务端输入边界拒绝写入。

## 6. 信息架构与交互要求

### 6.1 设置入口

位置：`设置 → 通用 → 外观 / Appearance`，与个人“界面语言”卡片并列，位于语言卡片之前。

卡片包含：

- 标题：外观；
- 说明：只影响当前管理员的 Admin Console；
- 两个选项：浅色、深色；
- 每个选项提供小型界面色板预览、名称和选中标记；
- 保存中、保存成功、保存失败状态；
- 中英文语义键必须完整对等。

桌面端可横向展示两个选项；窄屏按单列展示。选项整体均可点击，但内部不得形成重复或嵌套交互控件。

### 6.2 控件语义

优先使用原生 `fieldset`、`legend` 和 radio input；如果因预览样式使用自定义交互，必须实现标准 radiogroup 键盘行为：

- Tab 进入/离开组；
- 方向键移动选择；
- Space 选择；
- 焦点环在 Light 与 Dark 下均清晰；
- 屏幕阅读器可获知组名、选项名、选中状态和保存状态。

### 6.3 主题应用范围

Appearance 必须覆盖已登录 Admin Console 的全部界面，包括：

- 全局 Shell、侧栏、顶栏、页面背景；
- Dashboard、图表、指标、治理状态；
- Projects、Providers、Deployments、Routes、Policies；
- Usage、Operations、Audit、Alerts；
- Settings、MFA、恢复码、Master Key Custody；
- Modal、Notice、Tooltip（如有）、空状态、Loading、错误页和 404；
- 表格、表单、代码文本、徽标、状态点、滚动条和选择高亮；
- 二维码周围容器及打印/下载之外的展示区域。

业务状态的含义在两种主题中必须一致：Success、Warning、Danger、Info 不得因主题变化改变语义。

## 7. 设计系统规范

### 7.1 分层模型

设计系统至少分为三层：

1. **Primitive Token**：原始色阶、尺寸、字号、字重、行高、圆角、阴影、时长；
2. **Semantic Token**：背景、表面、文字、边框、操作、焦点和状态等意图；
3. **Component Token**：仅在基础组件无法由语义 Token 完整表达时使用，例如 Modal backdrop、Sidebar active、Chart grid。

业务页面只允许使用 Semantic Token 或已批准的 Component Token，不直接消费原始色阶。

### 7.2 Token 最小集合

颜色语义至少包含：

```text
color.canvas
color.surface.default
color.surface.subtle
color.surface.raised
color.surface.overlay
color.text.primary
color.text.secondary
color.text.tertiary
color.text.inverse
color.border.default
color.border.strong
color.action.primary
color.action.primary.hover
color.action.secondary
color.focus.ring
color.status.success.{text,surface,border,icon}
color.status.warning.{text,surface,border,icon}
color.status.danger.{text,surface,border,icon}
color.status.info.{text,surface,border,icon}
color.chart.{series-1,series-2,grid,axis,tooltip}
```

非颜色语义至少包含：

```text
font.family.{sans,mono}
font.size.{xs,sm,md,lg,xl,2xl}
font.weight.{regular,medium,semibold,bold}
line-height.{tight,normal,relaxed}
space.{0,1,2,3,4,5,6,8,10,12,16}
radius.{sm,md,lg,pill}
shadow.{raised,overlay,focus}
motion.duration.{fast,normal}
motion.easing.standard
z-index.{sticky,dropdown,modal,toast,skip-link}
```

Token 命名表达用途，不包含具体颜色名；现有品牌 `lime`、`mint` 可以保留在 Primitive 层，但业务组件应使用 `action`、`accent`、`success` 或 `chart.series` 等语义名。

### 7.3 CSS 契约

- 主题通过根元素 `data-appearance="dark|light"` 应用；
- `color-scheme` 必须随主题设置为对应值，使原生表单和滚动条匹配；
- Dark 和 Light 必须声明同一组完整 Semantic Token；
- 缺少 Token 时构建或测试失败，不允许依赖另一主题的偶然回退；
- 除透明黑白遮罩、二维码黑白和经过记录的特殊资产外，组件 CSS 不新增硬编码颜色；
- 页面不得读取 `data-appearance` 决定业务逻辑；
- 图标优先使用 `currentColor`，不得为两个主题维护两份等价 SVG。

建议目录边界：

```text
web/src/design-system/
  tokens.css
  themes/dark.css
  themes/light.css
  foundations.css
  components.css
  README.md
```

最终拆分可在技术设计中调整，但 Token、主题和业务页面之间的单向依赖不可取消。

### 7.4 基础组件契约

第一阶段纳入设计系统治理的基础组件：

- Button / IconButton；
- Input / Select / Textarea / Checkbox / Radio；
- Field / Form actions；
- Panel / Card / Detail panel；
- Badge / StatusDot / Health pill；
- Notice / Error state / Empty state / Loading；
- Modal；
- Tabs / Settings navigation；
- Table / Resource list / Pagination；
- PageHeader；
- Code / Monospace value；
- Chart frame / Legend / Tooltip。

每个组件必须定义：默认、hover、active、focus-visible、disabled、loading、selected 和 error（适用时）状态，以及尺寸、响应式行为和无障碍名称来源。

### 7.5 设计系统文档

仓库内提供面向开发者的 `README.md`，至少说明：

- Token 分层和命名规则；
- 新组件何时复用、何时扩展；
- Light / Dark 验证方法；
- 禁止硬编码颜色和禁止页面主题分支的示例；
- 可访问性检查清单；
- 新增或变更 Token 的评审要求。

第一版不强制引入 Storybook。优先用现有 Vite/Vitest 能力提供只在开发环境使用的设计系统展示页或组件测试夹具；该页面不得进入生产导航，也不得扩大生产 bundle。

## 8. Light 与 Dark 视觉要求

### 8.1 品牌连续性

- 保留 Halro 的克制、技术化、安全运维视觉语言；
- 品牌强调色在 Light 中应调整明度或搭配深色前景，不能原样套用导致低对比；
- 两种主题保持相同布局、组件尺寸、信息密度和图标，不因主题改变页面结构。

### 8.2 层级与边界

- Light 使用柔和非纯白 Canvas 与明确 Surface 层级，避免大面积高亮眩光；
- Dark 保持当前低亮度基线，但提升弱文本与弱边框中不满足对比度的部分；
- Card、Modal、Sidebar、悬浮层不能只依赖阴影区分，必须同时有可辨识边界或背景层级；
- 禁用态仍需可读，但不得与可操作态混淆。

### 8.3 图表与数据可视化

- 每个系列在两种主题下均具有足够对比；
- 多系列不能只依靠红绿或相近明度区分，应结合线型、点型或标签；
- 坐标轴、网格、Tooltip、空状态和选中范围均使用语义 Token；
- 图表截图和打印不是本期主题目标，但浏览器缩放 200% 时不得丢失核心标签或交互入口。

### 8.4 敏感与特殊内容

- TOTP 二维码保持黑色模块、白色底和足够静区，不随 Dark 反色；
- 恢复码、Key、Hash 和 Audit ID 的等宽文本在两种主题下清晰可选中；
- Danger 操作在两种主题下均同时使用文字、图标或结构提示，不只使用红色；
- 浏览器自动填充、密码管理器填充和文本选中颜色不得使凭据不可读。

## 9. 数据与 API 需求

### 9.1 AdminUser

在现有管理员偏好模型中新增：

```text
Appearance  "light" | "dark"
```

要求：

- 既有记录缺失或为空时读取为 `dark`；
- 新写入必须是受支持枚举；
- 数据迁移可惰性规范化，不要求停机批量重写；
- revision 与语言偏好共用，任一字段更新均递增；
- 备份、恢复和升级/降级兼容策略在技术设计中明确。

### 9.2 Preferences API

`GET /admin/api/v1/preferences`：

```json
{
  "locale": "system",
  "appearance": "dark",
  "revision": 4
}
```

`PUT /admin/api/v1/preferences`：

```json
{
  "locale": "system",
  "appearance": "light"
}
```

要求：

- 继续要求 Admin Session、CSRF 与 `If-Match`；
- 未知字段遵循现有 Admin JSON 严格解析规则；
- 缺字段不能静默清空另一个偏好；客户端必须发送完整资源；
- 无效 Appearance 返回稳定的本地化错误码，不保存部分变更；
- 响应返回新的完整资源与 revision / ETag。

### 9.3 Session 与启动

- Session 响应应包含规范化后的 `appearance`，使已认证应用尽早应用主题；
- Preferences 资源仍是设置页面的可编辑真相源；
- 登录与 MFA 成功响应返回 Appearance 时不得改变其现有敏感数据边界；
- 未认证 bootstrap 第一版不新增 Appearance 字段，默认 Dark。

## 10. 状态、并发与安全边界

### 10.1 前端状态机

```text
server-confirmed
  → previewing/saving
  → saved
  → server-confirmed

previewing/saving
  → failed
  → rollback to server-confirmed
```

- React 内存状态保存当前预览和最后已确认值；
- 不在 DOM 属性中写入用户名、revision 或其他账户信息；
- 只在根元素写 `data-appearance` 枚举；
- 卸载或登出时清理个人状态；
- 异步响应必须关联请求顺序，旧响应不得覆盖新选择。

### 10.2 安全要求

- 不增加第三方字体、图标、样式 CDN 或主题服务网络请求；
- 不增加浏览器持久化 API；
- 保持现有 CSP、同源、CSRF、Session 和 bundle Secret Canary 门禁；
- 主题、设计系统展示和截图测试不得包含真实 Credential、Gateway Key、恢复码或生产 Audit 内容；
- CSS 生成内容不得承载安全关键信息；
- `forced-colors` 下不能隐藏焦点、选中、错误或危险操作语义。

## 11. 可访问性与质量标准

### 11.1 对比度

两种主题均满足：

- 普通文字与背景至少 `4.5:1`；
- 大号文字至少 `3:1`；
- 组件边界、焦点指示和关键非文字图形至少 `3:1`；
- Disabled 的豁免不用于普通说明文字、只读值或不可用原因；
- Success / Warning / Danger 的文字和图标分别检查，不能只验证色块。

### 11.2 键盘与焦点

- 主题切换后焦点仍停留在所选控件；
- Modal focus trap、skip link、导航、Tabs、表格操作和表单错误定位不得回归；
- `:focus-visible` 在所有 Surface 上清晰可见；
- 不使用仅 hover 才出现的关键操作。

### 11.3 动效

- 主题切换可使用不超过 `150ms` 的颜色/背景过渡；
- 不对布局、尺寸和整页 opacity 做切换动画；
- `prefers-reduced-motion: reduce` 时移除非必要过渡；
- 首次加载不得播放全页面主题动画。

### 11.4 响应式与缩放

覆盖至少：

- 320px 窄屏；
- 768px 平板宽度；
- 1280px 与 1440px 桌面；
- 浏览器 200% 缩放；
- 中英文长文案；
- 空数据、长 ID、错误、Loading、Modal 与表格溢出场景。

## 12. 实施范围与迁移策略

### 12.1 阶段 A：盘点与基线

- 建立页面、组件、状态、硬编码颜色和现有 CSS 变量清单；
- 固定升级前 Dark 关键页面截图和交互测试；
- 标记重复样式、一次性页面样式和应提取的基础组件；
- 确定 Token 命名和变更规则。

### 12.2 阶段 B：设计系统基础

- 建立 Primitive、Semantic、Component Token；
- 迁移字体、间距、圆角、层级、焦点、状态和动效基础；
- 整理基础组件状态，保持业务行为与 API 不变；
- 添加 Token 完整性和禁止新增硬编码颜色的自动检查。

### 12.3 阶段 C：Dark 迁移

- 先让现有界面全部只通过新 Token 呈现；
- 完成关键路径视觉对比与交互回归；
- 对原有不满足 AA 的颜色允许做最小必要修正，并记录变化；
- 在 Dark 回归通过前不开放 Appearance 控件。

### 12.4 阶段 D：Light 与 Appearance

- 提供完整 Light Token；
- 扩展 Admin 偏好数据和 API；
- 实现根主题控制器、启动/登出恢复和设置卡片；
- 完成所有页面、状态和响应式双主题 QA。

### 12.5 阶段 E：收口

- 清理被替代的旧变量和无调用样式；
- 更新用户指南、实现状态和设计系统开发文档；
- 记录剩余例外及负责人，不允许用永久通配豁免关闭门禁；
- 生产 bundle 检查确认开发展示页、测试夹具和截图资产未被打包。

## 13. 测试与验收

### 13.1 自动化测试

至少包含：

- Appearance 枚举规范化、默认值和持久化单元测试；
- Preferences GET/PUT、CSRF、ETag、revision 冲突、无效值和 Audit 测试；
- Session、登录、MFA 完成和登出主题应用测试；
- Settings radio 键盘、即时切换、成功、失败回滚和乱序响应测试；
- 中英文资源键结构完全对等测试；
- Light / Dark Token 键集合完全相同测试；
- 禁止新增未批准硬编码颜色的静态检查；
- 现有 accessibility、页面和组件测试在两个主题参数下运行；
- bundle 不包含 Local Storage、Session Storage、IndexedDB 及开发展示资产的既有/新增检查。

### 13.2 视觉回归矩阵

每种主题至少覆盖：

| 类别 | 必测状态 |
|---|---|
| Shell | 默认、活动导航、窄屏导航、账号卡片 |
| Dashboard / Usage | 有数据、空数据、图表 Tooltip、Warning / Danger |
| 资源页面 | 列表、详情、表单、Disabled、Error、删除确认 |
| Operations / Audit | 表格、筛选、长 ID、状态 Badge |
| Settings | Appearance、语言、MFA、恢复码、诊断 |
| Overlay | 普通 Modal、危险 Modal、Loading、错误 Notice |
| Authenticated fallback | 404、API 失败、偏好加载失败 |

视觉差异阈值、基线更新审批和 CI 运行环境由技术设计固定；不得通过扩大整页容差掩盖真实回归。

### 13.3 人工验收

- Chrome、Safari、Firefox 当前受支持版本；
- 键盘全流程和至少一种桌面屏幕阅读器冒烟；
- 320px、桌面和 200% 缩放；
- `prefers-reduced-motion` 与 `forced-colors`；
- 浏览器自动填充、复制、文本选中和原生表单；
- 主题保存后登出/登录及另一浏览器恢复；
- 网络失败、409 revision 冲突和快速连续切换。

## 14. 验收标准

以下条件全部满足才可标记完成：

1. 所有已登录 Admin 页面可在 Light 与 Dark 下完整使用，无明显未主题化区域。
2. 现有 Dark 的品牌、布局和业务交互无非预期回归。
3. Appearance 只提供 Light / Dark，默认 Dark，并可从“设置 → 通用”即时切换。
4. 偏好服务端持久化，刷新、重新登录和跨设备后恢复；浏览器持久化 API 未被使用。
5. 保存失败自动回滚，revision 冲突不会静默覆盖其他会话的更新。
6. 登出和未认证页面使用 Dark，不泄露前一管理员的主题偏好。
7. 双主题关键文字、组件边界和焦点满足 WCAG 2.2 AA 对比度要求。
8. 图表、状态、错误和危险操作不只依赖颜色传达含义。
9. Light / Dark Token 集合一致，业务 CSS 不新增未批准硬编码颜色或主题名称分支。
10. 中英文文案对等，主题切换不刷新、不丢焦点、不丢表单或查询状态。
11. 自动化、视觉、响应式和人工验收矩阵通过。
12. 设计系统开发文档、用户指南与实现状态已更新。

## 15. 发布与回滚

- 数据模型先向后兼容发布：缺失 Appearance 始终读取为 Dark；
- 前端在服务端能够读取/写入 Appearance 后才显示设置控件；
- 如 Light 出现发布阻断问题，可通过版本回滚隐藏控件并回到 Dark，已有 `light` 偏好数据保留且不破坏读取；
- 回滚不得要求删除管理员偏好或降级数据文件；
- 发布说明必须明确：未认证页面仍为 Dark，System 模式不在本期。

## 16. 成功指标

发布后观察：

- Appearance 偏好保存 API 的成功率与 revision 冲突率；
- 前端主题应用错误和未捕获异常不得高于发布前基线；
- 双主题可访问性自动检查零新增严重问题；
- 新增业务组件的硬编码颜色门禁零违规；
- Light 与 Dark 下关键 Admin 任务完成率不因主题产生显著差异。

不采集管理员的操作系统主题、浏览器指纹或跨站行为。主题选择分布若无现有、经批准的匿名产品遥测能力，则不新增遥测系统。

## 17. 风险与缓解

| 风险 | 缓解措施 |
|---|---|
| 单体 CSS 中硬编码颜色造成 Light 漏洞 | 先完成 Dark Token 迁移，静态检查禁止新增硬编码颜色 |
| Light 强调色对比度不足 | 为 Light 定义独立语义值，逐状态进行对比度检查 |
| 异步保存导致主题跳回或旧响应覆盖 | 使用已确认基线与请求序列，失败回滚，revision 冲突重新获取 |
| 登录前后主题发生一次变化 | 第一版明确未认证 Dark；应用已认证主题时不做整页动画 |
| 两个主题使视觉回归矩阵翻倍 | 建立代表性页面矩阵、稳定截图环境和 Token 级检查 |
| 组件抽取引入业务行为回归 | 迁移阶段不改变 API 和信息架构，保留现有交互测试 |
| 设计系统成为无约束 CSS 集合 | 明确分层、组件契约、文档和新增 Token 评审规则 |

## 18. 待技术设计确认项

以下项目不改变本 PRD 的产品边界，但需在开发前形成技术设计记录：

1. Token 文件的最终目录、构建导入顺序和命名映射；
2. 当前 CSS 拆分粒度以及遗留类名的迁移批次；
3. 视觉回归工具、浏览器版本、截图分辨率和差异阈值；
4. 硬编码颜色检查的允许清单格式；
5. AdminUser 序列化、备份/恢复和旧版本降级对新增字段的具体行为；
6. Preferences PUT 由完整资源更新还是增加等价的安全 PATCH；若改变现有契约，必须避免字段丢失并保持 revision 语义；
7. Session 响应与 Preferences 请求之间的去重及首个已认证页面主题应用时序。

## 19. 后续候选

不纳入本期验收，但设计系统不得阻碍：

- `System` Appearance 与操作系统主题实时跟随；
- 实例级未认证页面默认 Appearance；
- 高对比度主题；
- 设计 Token 导出给文档站或其他 Halro 管理界面；
- 独立组件展示与视觉评审环境；
- 用户可选的信息密度与减少透明效果偏好。

# Foundations

实现入口为 `web/src/design-system/index.css`，依赖顺序固定为 Primitive → Dark semantic → Light semantic → shared components → page CSS。业务代码不得读取 `--p-*` 或直接写颜色字面量。

## Typography

| 角色 | Token / 值 | 用途 |
| --- | --- | --- |
| Page title | `--type-page-title-size` 32px；窄屏 `-compact` 28px | 每页唯一 H1 |
| Section title | `--type-section-title-size` / 22px | 页面一级区块 |
| Card title | `--type-card-title-size` / 18px | Panel、Drawer、卡片标题 |
| Body | `--type-body-size` / 15px | 描述与长文本 |
| Label | `--type-label-size` / 13px | 控件标签、事实值 |
| Caption floor | `--type-caption-size` / 12px | 标签、时间、图表轴；禁止更小 |
| Metric | `--type-metric-size` / clamp(20px, 2vw, 30px) | 关键聚合值，普通字段不得使用 |
| Display / Hero | `--type-display-size` 34px、`--type-hero-size` clamp(34px, 4.2vw, 46px) | **仅** Login / Setup 入口屏；控制台页面使用即缺陷 |

默认使用系统 Sans；ID、代码与数值轴使用系统 Mono。粗细仅使用 400/500/600/700。中文依赖 PingFang SC、Microsoft YaHei、Noto/Source Han 回退。

## Color

Primitive 只定义于 `tokens.css`。组件只消费 `--color-text-*`、`--color-surface-*`、`--color-border-*`、`--color-action-*`、`--color-status-*`、`--color-chart-*` 等语义角色。Dark/Light 必须键集合一致；正文对比不低于 4.5:1，关键边界不低于 3:1。

状态色共六族：`success / warning / danger / info / neutral / unknown`，各自拥有 text / surface / border / icon。

- `neutral` 表达"管理员主动关闭"——它是状态不是故障，不得借用 danger 或 warning 的外观。
- `unknown` 表达"读不出来"，与 `neutral` 的区分是结构：虚线边界 + 无填充，对应 `neutral` 的实线边界 + 底色。状态本就不允许只靠颜色表达，所以结构差异是区分本身，不是替代品。
- 三值状态必须为每个值写出各自的处理，基础规则不得携带任何状态处理——否则新增第四个值会静默继承其中一种。

**Disabled**：使用 `--color-action-disabled` / `--color-action-disabled-border`，不用 `opacity`。透明度让对比度取决于控件恰好叠在什么背景上，同一写法在本仓库曾散落在 1.90:1 到 6.93:1 之间。唯一例外是原生复选框（不承载文本，UA 自绘 disabled）。

**链接**：正文链接使用 `.text-link`（`--color-text-link` + 下划线）。颜色是扫读可供性，下划线是 forced-colors 与色觉差异下仍然存在的那一半。全局 `a { color: inherit }` 保持不变——导航、设置分区与按钮型锚点依赖继承。每个锚点都必须有一个说明它是什么的 class。

## Spacing、Grid 与尺寸

- 4px 基础尺度：`--space-1` 至 `--space-16`。组件内部节奏默认直接取这个阶梯。
- 页面外框：`--layout-page-inline: clamp(16px, 4vw, 64px)`、`--layout-page-block-end: 72px`。
- Shell 侧栏：`--layout-sidebar-width: 248px`；内容宽度按页面模式约束。
- 资源行节奏由 `--resource-row-padding-block` / `-inline` / `--resource-cell-gap` 承担，不另造通用行内边距 Token。
- 桌面控件可用 `--control-block-size: 36px`；820px 以下交互目标使用 `--control-touch-block-size: 44px`。
- 页面不新增裸 spacing/radius；现有债务由 `design-system.test.ts` 的精确基线 691 约束，只能在审阅后下降或明确解释变化。

**Token 升格规则**：一个值只有在**必须跨多个组件同步变化**时才升格为语义 Token。与 `--space-*` 值相同的别名是重复概念，不是角色。任何新增的 `--color-* / --type-* / --layout-* / --control-*` 角色必须同时有调用点——`declares no semantic role the product never reads` 精确断言这一点，声明一个没人读的角色会直接让门禁失败。

## Radius、Elevation、Motion

- Radius 只用 `--radius-sm/md/lg/pill`；高风险操作不因更大圆角显得轻松。
- Elevation 只用于菜单、浮层、Modal/Drawer；普通信息区优先边界与表面层级。
- 动画使用 fast 90ms / normal 150ms 与 standard easing；`prefers-reduced-motion` 下取消非必要动画。

## Responsive modes

| 模式 | 典型范围 | 规则 |
| --- | --- | --- |
| Wide | >1120px | 固定侧栏，多列主从布局 |
| Regular | 821–1120px | 降低列数，保留侧栏 |
| Compact | 581–820px | 顶部 disclosure 导航、单列内容、44px 控件 |
| Small | 320–580px | 28px 标题，事实/操作重排，表格改卡片或局部滚动 |

760/720/640 等规则只可作为组件内容断点；不得再创造新的全局壳层模式。

## Icon 与数据可视化

图标使用 24×24 viewBox、currentColor、统一描边；图标按钮必须有可访问名称。图表轴字号使用 12px Token，系列同时使用颜色与线型/标签；无流量不得绘成 100% 成功。

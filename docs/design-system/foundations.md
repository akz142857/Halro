# Foundations

实现入口为 `web/src/design-system/index.css`，依赖顺序固定为 Primitive → Dark semantic → Light semantic → shared components → page CSS。业务代码不得读取 `--p-*` 或直接写颜色字面量。

## Typography

| 角色 | Token / 值 | 用途 |
| --- | --- | --- |
| Page title | `--type-page-title-size` 32px；窄屏 28px | 每页唯一 H1 |
| Section title | `--type-section-title-size` / 22px | 页面一级区块 |
| Body | `--type-body-size` / 15px | 描述与长文本 |
| Compact label | `--font-size-sm` / 13px | 控件、事实值 |
| Caption floor | `--type-caption-size` / 12px | 标签、时间、图表轴；禁止更小 |

默认使用系统 Sans；ID、代码与数值轴使用系统 Mono。粗细仅使用 400/500/600/700。中文依赖 PingFang SC、Microsoft YaHei、Noto/Source Han 回退。

## Color

Primitive 只定义于 `tokens.css`。组件只消费 `--color-text-*`、`--color-surface-*`、`--color-border-*`、`--color-action-*`、`--color-status-*`、`--color-chart-*` 等语义角色。Dark/Light 必须键集合一致；正文对比不低于 4.5:1，关键边界不低于 3:1。

## Spacing、Grid 与尺寸

- 4px 基础尺度：`--space-1` 至 `--space-16`。
- Shell 侧栏：`--layout-sidebar-width: 248px`；内容宽度按页面模式约束。
- 桌面控件可用 `--control-block-size: 36px`；820px 以下交互目标使用 `--control-touch-block-size: 44px`。
- 页面不新增裸 spacing/radius；现有债务由 `design-system.test.ts` 的精确基线 693 约束，只能在审阅后下降或明确解释变化。

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

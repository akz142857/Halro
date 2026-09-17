# Component contracts

> 本文出现的 Wide / Regular / Compact / Small 指 `foundations.md` 的响应式模式定义（> 1120 / 821–1120 / 581–820 / 320–580px），不要按其他文档的同名词理解。

组件实现主要位于 `web/src/components.tsx`、`web/src/design-system/*.css` 与共享页面组件。新增页面应组合现有契约，不复制视觉规则。

| 组件族 | Anatomy / variants | 状态 | Keyboard / responsive contract |
| --- | --- | --- | --- |
| Button / IconButton | primary、ghost、danger；label/icon | default、hover、focus、disabled、pending | 原生 button；窄屏目标 44px；图标按钮必须有 label |
| Field / Select / Combobox | label、control、hint、error | required、invalid、disabled、loading | label 关联；Combobox 使用箭头/Enter/Escape；列表不越出视口 |
| PageHeader / Section / Panel | eyebrow、title、description、action | normal、warning/read-only context | 每页单 H1；Small 下 action 换行，不覆盖标题 |
| Badge / Notice / Empty / Error / Loading | icon/state、title、body、action | success、info、warning、danger、unknown | 不只靠颜色；错误持久；loading 有可读名称 |
| ResourceRow / ResourceCard / Table | identity、facts、state、actions、details | selected、expanded、partial、disabled | identity 优先；Small 重排或卡片化；只有表格容器可横向滚动 |
| Tabs / SegmentedControl / Menu | tablist、tab、panel | selected、focus、disabled | Tabs 支持方向键；Small 自动换行/网格，不隐藏目的地 |
| Modal / Drawer / ConfirmDialog | title、description、body、actions | normal、dirty、dangerous、pending | 焦点陷阱、Escape 取消、关闭后恢复焦点；危险确认不响应 backdrop |
| Notification | tone、title、optional action | success/info 自动消失；error 保留 | `aria-live`；语言使用动作完成后的当前 locale；不承载秘密 |
| Metric / Chart / Timeline / CodeBlock | label、value、context/evidence | empty、partial、stale、error | 12px 字号底线；图表有文本摘要；代码局部滚动/换行 |

## Navigation contracts

- Wide/Regular 使用固定侧栏。
- Compact/Small 使用带 `aria-expanded`、`aria-controls` 的顶部 disclosure；关闭时不占内容高度，打开后所有目的地与账户动作可见且纵向滚动。
- Settings 在 Compact/Small 使用原生 Select 显示当前分区；桌面使用垂直导航。两者来自同一 `SETTINGS_PANES` 数据源。
- 离页保护使用共享 `Modal` 的 `alertdialog`，绝不使用 `window.confirm`。

## State checklist

组件改动至少验证 default / hover / focus-visible / disabled / loading / error / selected（适用时），并覆盖 Light/Dark、中文/英文、窄屏、reduced-motion 与 forced-colors。行为变更必须有直接测试。

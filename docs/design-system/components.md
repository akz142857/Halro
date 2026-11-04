# Component contracts

> 本文出现的 Wide / Regular / Compact / Small 指 `foundations.md` 的响应式模式定义（> 1120 / 821–1120 / 581–820 / 320–580px），不要按其他文档的同名词理解。

组件实现主要位于 `web/src/components.tsx`、`web/src/design-system/*.css` 与共享页面组件。新增页面应组合现有契约，不复制视觉规则。

| 组件族 | Anatomy / variants | 状态 | Keyboard / responsive contract |
| --- | --- | --- | --- |
| Button / IconButton | primary、ghost、danger；label/icon | default、hover、focus、disabled、pending | 原生 button；窄屏目标 44px；图标按钮必须有 label |
| Field / Select / Combobox | label、control、hint、error | required、invalid、disabled、loading | label 关联；Combobox 使用箭头/Enter/Escape；列表不越出视口 |
| PageHeader / Section / Panel | title、可选 eyebrow/description/action | normal、warning/read-only context | 每页单 H1；无说明不生成空段落；Small 下 action 换行，不覆盖标题 |
| DisclosureSection | title、配置 summary、native summary/details、body | collapsed、expanded、invalid | 内容保持挂载；摘要可键盘操作；校验失败展开祖先，按表单顺序聚焦首个错误；重复提交仍可恢复 |
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

## 折叠配置与表单帮助

- `DisclosureSection` 用于专业配置，不能用来隐藏完成主任务所需的接口、凭据、模型或预算范围。
- 收起不卸载字段，保留值、dirty 和校验状态；摘要须包含足以判断是否偏离默认配置的事实。
- 错误或原生 invalid 会展开所在区块；多个区块同时失败时聚焦整个表单的首个错误，不能由最后一个区块抢焦点。
- Modal 焦点循环包含 summary，跳过闭合 details 的后代，保留 Tab、Shift+Tab、Escape、取消和离页保护。
- 采用 roving tabindex 的 Tabs 仅当前选项进入 Modal 的 Tab 范围；复制等其他操作禁用时也不能让焦点逃到背景。
- Field 的 label 始终保留；短 hint 只解释新增约束。错误通过 `aria-describedby` 关联，替代普通 hint，不让读屏同时读到两份说明。
- 代理信任摘要在选择代理后显示；TLS/CA 完整边界可展开。上游自行联网风险独立判断，不能跟着代理提示一起隐藏。

## 提示与表单间距

- 网格和弹性容器由 `gap` 管理相邻元素间隔；内部 `Field` 和 notice 不再叠加块外边距。普通块布局的提示保留自身前后分隔，内嵌面板和末尾提示由容器 inset 收口。
- 价格表单包含多个独立网格，仍需保留分组外侧间隔；不能因清除字段边距而将各组贴合。
- 诊断卡片的标题、普通说明、表格和依据保持同一水平 inset。嵌套 details 使用自己的摘要样式，避免继承外层卡片标题的布局。

# Page patterns

> 本文出现的 Wide / Regular / Compact / Small 指 `foundations.md` 的响应式模式定义（> 1120 / 821–1120 / 581–820 / 320–580px），不要按其他文档的同名词理解。

## Overview

结构：PageHeader → 关键 Metric → 趋势/异常 → 次级活动。指标先回答“是否正常”，图表回答“何时变化”，明细回答“为什么”。空数据不伪装为健康。

## Resource list

结构：PageHeader → tabs/filter/toolbar → ResourceRow/Card → detail/modal。对象 identity 永远高于属性；运行状态和主操作在首层可见。Small 下事实换行或隐藏到 disclosure，但名称、状态和主操作不得隐藏。

## Master-detail

Regular/Wide 可并排；Compact/Small 堆叠或使用 Drawer。选中项保持可见，关闭详情恢复焦点和滚动上下文。长 ID 在自身容器处理，不扩大页面。

## Settings

Wide/Regular 使用左侧分区导航，Compact/Small 使用 Select。每个分区先说明作用域（个人、实例、账户），再呈现表单；保存反馈只出现一次，且与切换后的 locale 一致。启动配置和可热更新设置不得混在同一编辑面。

## Data table

优先保留语义 `<table>`。列语义允许时，Small 转为带 `data-label` 的卡片行；高密度技术表保持局部横向滚动，并让当前任务所需列和操作处于首屏。禁止由 body 承担横向滚动。

## High-risk action

触发器说明动作；ConfirmDialog 明确对象、后果与不可逆性；服务端拒绝在对话框内持久显示。Escape 永远取消，不执行确认。Read-only 原因应在控件附近可读。

## Login / Setup

这是入口任务，不是营销页。保留品牌与信任边界，但标题不得挤压表单；320px 和 400% 缩放下表单、语言切换、错误与恢复入口都必须可达。

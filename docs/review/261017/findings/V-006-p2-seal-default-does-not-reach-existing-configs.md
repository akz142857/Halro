# V-006 — P2：封存默认开启到不了已有配置，`main` 上也没有任何说明

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 M03（#421 `4ad9c844`）× F06 配置迁移；交叉点 X15
- 分类：【问题】，运行时证据（E01）

## 现象

#421 把 `ledger.seal` 默认改为 `enabled: true`、`max_active_bytes: 268435456`，理由是"打开 Ledger 要读完整个活动代，它的大小决定启动时间"。但 v0.8.5 的 `configs/config.example.yaml` **显式写出** `enabled: false`、`max_active_bytes: 8589934592`；从示例复制的配置经 `halro config migrate --write` 后仍是这两个值（迁移按设计不改动操作者设置的值，E01 实测）。

于是从 v0.8.5 升级的典型安装得不到这项改动，启动时间继续随活动 WAL 增长——而这正是 P2 要解决的问题之一。

`main` 的 CHANGELOG `[Unreleased]` 中没有任何关于封存默认值的条目（见 V-007）；该说明只存在于未合并的发布准备分支 `release/v0.9.0`（#426）。

## 建议

二选一并写进升级说明：在 `config migrate` 中把"等于旧默认值"的显式封存设置迁到新默认（需要明确这是对操作者值的一次有意改写），或保留现状但让 `doctor`/启动日志在 `enabled: false` 且活动 WAL 超过新默认时给出 warn（与 V-005 合并）。

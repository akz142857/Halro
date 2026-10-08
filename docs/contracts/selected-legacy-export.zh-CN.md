# Selected 状态的最新内容回退候选

`halro metadata export-legacy --config source.yaml --output /absolute/absent-candidate`
要求已停止的 Standalone、已激活的七根 `memory-state`、File Master Key，以及校准后的 `resource_budget` / `resource_pressure`。HA 与 Key Slots 模式拒绝。输出目录必须不存在，且不能与源数据目录互相包含；不会导入操作者提供的旧备份。

命令从当前 selected Metadata 完整认证行树和实际原生日志现场创建 format5 备份。随后在独立目录内复制 Master Key，调用完整备份恢复协议重建当前 schema 的 legacy reader 状态，再实际执行当前构建的 Runtime Open / Close。Metadata、全部 Ledger sealed generations / active WAL、Audit、Governance、价格、幂等和被备份的保留对象由已有完整备份/恢复验证门禁覆盖。无权读取的 prefix、损坏、未知压力、资源不足或失败的真实 Close 均不报告成功。

输出包含：

- `latest.hmbk` 与私有的 `archive.key`：本次现场认证源；
- `master.key` 与 `data/`：独立目标密钥和可运行目录；
- `config.yaml`：指向独立 data/key/log 文件的目标配置；
- `verified-reader.json`：本次 Backup ID、schema、Ledger watermark、构建信息及实际 Runtime 验证结果。

目标沿用显式 Restore 语义：撤销管理员会话，隔离跨维护窗口的 scheduled prices，追加 restore 审计，并由真实 Runtime 完成必要恢复。TLS、Setup Token、Metrics 和 Audit Anchor 凭据仍是只读配置引用。验证不绑定 HTTP listener，也不调用真实模型 provider。源的业务事实和密钥不切换；正常现场 backup 审计及 checkpoint 仍会记录在源中。

这是本次 snapshot 的候选，不能在源又接受新流量后直接拿它覆盖源。验证 receipt 只证明当前构建的 legacy reader 能读当前 schema；没有为任意旧镜像、旧 schema 或不同 feature set 作兼容声明。选择旧发布版本仍需独立的完整 reader 兼容证据。

若需要轮换，先对这个独立目标执行现有命令：

```sh
halro key rotate --config /absolute/candidate/config.yaml --new-key-file /absolute/new-master.key
```

此路径只改独立目标。已有 staged Vault rewrite、发布桥、Master Key 替换和中断重试协议负责真实密钥轮换；原 selected 目录仍能以原 key 重开。若目标仍保留 failure captures 或 provider objects 的 Vault 密文，现有轮换边界继续拒绝，须先由其正常 retention 生命周期移除，不能只重加密 Metadata 后遗失保留对象。完成后如需再次采用七根，应以目标最新完整备份重新执行显式 `metadata migrate --activate-runtime`，不得复用源旧 seal 或根文件。

失败候选保留用于检查。相同 `--output` 重试拒绝，避免悄悄复用过时 snapshot；重试 export 须选新的不存在目录。轮换阶段自身可按现有协议重试。该流程串行执行三个已退休的实际 owner 阶段，每阶段沿用现有独立 manager 与压力门禁；尚无覆盖整个 export/restore/Runtime pipeline 的统一物理空间预留。selected 原目录的原地 DEK 轮换也仍待独立多文件发布协议，普通 `RotateMasterKey` 在 selected 目录继续拒绝。

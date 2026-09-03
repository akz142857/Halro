# 260903 运行时证据

跑真二进制、对真数据目录取的证据,不是读代码得出的推论。全部在隔离目录里做,
未触碰仓库根的 `data/`、`master.key`、`config.yaml`,也未触碰操作员正在运行的实例。

- 被测二进制:`make build` 于 `ff12842`,`halro version` 报
  `v0.5.0-6-gff12842` / `2026-09-03T10:29:45Z`。
- 对照二进制:`v0.5.0` 标签在 git worktree 中 `go build -trimpath` 得到。
- 两份数据目录:一份用新二进制 `init`,一份用 v0.5.0 二进制 `init`(各自用**自己
  那一版**的 `configs/config.example.yaml`,端口改到 18080+ 以避开运行中的实例)。

## R1 — 升级路径上,`doctor` 与 `start` 对同一份数据目录判断相反

v0.5.0 建的数据目录是 bbolt schema **v33**;本 main 要求 **v35**。

```
$ ./bin/halro doctor --config <v0.5.0 建的实例>
exit=1   healthy=false
  [fail] metadata: metadata schema version 33 does not match required version 35

$ ./bin/halro start --config <同一份>
Halro is running        ← 12 秒后仍在监听,未拒绝

$ ./bin/halro doctor --config <同一份,start 之后>
exit=0   healthy=true
  [pass] metadata: bbolt schema v35
```

机制是设计使然,不是缺陷:`internal/store/bolt/store.go:1389` 的 `OpenReadOnly`
要求 `version != schemaVersion` 即报错——诊断命令必须只读,不能迁移;而
`openWithMigrationHook`(`store.go:1716`)在启动时 `for currentVersion < schemaVersion`
逐级迁移。两条路径分工正确。

**问题在措辞和它引导的操作。** 运维手册教的顺序是升级前先 `doctor`。一份完全正常、
只是尚未升级的数据目录,在这一步得到的是 `fail` + `healthy:false` + 退出码 1 ——
读起来是"这份数据坏了",而正确动作恰恰是继续启动。`doctor` 有能力分辨这两件事:
它读到的版本号 33 小于 35,是"待迁移",不是"不匹配"。

这与 260901 的 P15(`ledger verify` 把"没有"说成"不能认证")、P16(`ledger verify`
静默单向迁移)是同一族:诊断命令在"尚未"与"损坏"之间不作区分。本轮是第三处。

**级别:P1。** 不损坏数据,但会在升级的第一步把一次正常升级读成一次事故;若运维
反过来相信 `start` 的沉默,则又完全不知道数据目录已被单向改写。

## R2 — 迁移不可逆,且没有任何一处提示这件事

```
$ <v0.5.0 二进制> doctor --config <已被新二进制 start 迁移到 v35 的目录>
exit=1   healthy=false
  [fail] metadata: metadata schema version 35 does not match required version 33
```

`start` 迁移前不备份、不提示、不要求确认,迁移后也不在输出里说自己迁移过。回滚到
v0.5.0 的唯一路径是从升级前的备份恢复——而运维在升级时并不知道自己需要那份备份。

**级别:P1。** v0.6.0 的发布说明必须写死:升级前 `backup create`,且回滚只能靠它。
这也让 260901 P16 的缓解措施("在发布说明写死操作顺序")从建议变成必需。

## R3 — 全新实例上,五条诊断有两条以失败退出

一个刚 `init`、`doctor` 判 `healthy:true` 的实例上:

```
$ ./bin/halro config check   exit=0   configuration valid
$ ./bin/halro audit verify   exit=0   {"records":0,...}
$ ./bin/halro ledger verify  exit=1   halro: ledger chain could not be authenticated
$ ./bin/halro usage verify   exit=1   halro: verify usage: open <data>/usage/manifest.json:
                                      no such file or directory
```

同一份数据目录,`doctor` 对这两项的措辞是正确的:
`ledger` → `"committed sequence 0 at offset 0; no authenticated frames yet"`,
`parquet` → `"no usage manifest yet"`。所以信息是有的,只是两个独立命令没有用它。

`ledger verify` 这一条是 260901 的 **P15**,当时记为"v0.3.0 记过,第三次观测";
这是**第四次**。`usage verify` 把一个尚未产生的文件报成裸的 `open ...: no such
file or directory`,连命令自己的语义都没包一层。

**级别:P2。** 装完就跑一遍验证是运维的正常动作,五条里两条红,会训练人忽略这
两条的输出——而它们正是出事时唯一该被相信的两条。

## R4 — 回滚时旧二进制会拒绝新配置

先于 R2 观察到,单独记一条,因为它发生在更早的一步:

```
$ <v0.5.0 二进制> init --config <本 main 的 config.example.yaml>
exit=1
  - line 313: field failure_capture not found in type config.Gateway
  - line 597: field error_file not found in type config.Logging
```

配置不与默认值合并、逐键拒绝(这是有意的设计),所以回滚 v0.5.0 的运维除了要恢复
数据目录,还要把 `gateway.failure_capture` 与 `logging.error_file` 两段从
`config.yaml` 里删掉。反方向是安全的:本 main 接受 v0.5.0 的配置
(`config check` exit=0),新键缺省即关闭。

**级别:P3(文档)。** 只需在发布说明的回滚段落列出这两个键。

## 未能取得的证据

- **真实服务商行为**:计费且需外部凭据,按 `docs/verification/provider-real-matrix.md`
  的约定不在评审内触发。延迟取回、失败捕获在真实上游下的表现未验证。
- **性能回归**:未测。260901 的 **P14** 记录 `performance-baseline.md` 对路由解析
  差 12 倍、已不能作回归判据,在它被修正之前,本轮任何性能数字都无法与基线比较。
  这不是"性能没问题",是"这一轮没有可用的判据"。
- **崩溃恢复**:未在本轮的延迟取回与留存清扫路径上做真实崩溃注入,仅由测试盲区
  角色从测试覆盖角度评估。

# V4 · 对抗验证:B4-F2(rc.1 无升级路径)与 B4-F1(CHANGELOG 零公告)

任务:证伪,默认原发现是错的。模型:Fable 5(claude-fable-5)。无拒答、无空响应。
所有实跑在会话 scratchpad 隔离实例(`…/scratchpad/V4/`,端口 26310-26322),两枚二进制均独立重建(HEAD `2cd24a7` → `halro-head`;`v1.0.0-rc.1` 标签经 `git archive` 展开后构建 → `heimdall-rc1`),不复用 B4 的产物。

## 裁决

| 条目 | 裁决 | 严重度判断 |
|---|---|---|
| B4-F2(rc.1 → 1.0.0 无升级路径,三处断裂无公告,两处误导性失败) | **CONFIRMED** | P1/需裁决 **准确,且原判还漏了一处加重情节**(见 §3) |
| B4-F1(区间 22.5k 行改动 CHANGELOG 零提交,迁移 24~27 未公告,实例无感) | **CONFIRMED** | P1/阻塞 **准确** |

我按任务给出的每一条证伪方向逐一尝试,全部失败(§4)。唯一与原文有出入的是一处无关紧要的实跑构造差异(§2.4)。

## 1. 代码级复核:三处断裂的因果链是否真实

逐步走过原发现主张的路径,全部成立:

1. **备份域串**。HEAD `internal/backup/crypto.go:206` `domain := []byte("halro:backup:v1\x00")`;rc.1 同文件同行是 `heimdall:backup:v1\x00`(`git show v1.0.0-rc.1:internal/backup/crypto.go`)。该域串与 32 字节备份密钥拼接后 SHA-256 派生 AES-256-GCM 密钥(`backupAEAD`,crypto.go:202-218),故同一把密钥在两个版本派生出不同 AEAD 密钥;解密失败统一报 `backup authentication failed`(crypto.go:186)。调用链:`backup verify/restore` → `readEncryptedRecord`(crypto.go:170-200)→ `aead.Open` 失败。
2. **vault HKDF 串**。HEAD `internal/vault/vault.go:146` salt=`halro:vault:v1`、info 前缀 `halro:`;rc.1 同位置为 `heimdall:vault:v1`/`heimdall:`。key-check 明文/受众也换名(`internal/app/init.go:129-133`:`halro:metadata`、`halro-key-check-v1`)。调用链:`start` → `verifyVaultKeyCheck`(`internal/app/runtime.go:933-946`)→ `DecryptCredential` 失败 → **`master key does not authenticate the metadata store`**(runtime.go:940)。
3. **无任何兼容回退**:HEAD 的 Go 代码树中 `heimdall` 零命中(`grep -rn heimdall internal/ cmd/ --include='*.go'` 无输出)——不存在"先试新串、失败再试旧串"的迁移逻辑。改名提交 `0814cac`(2026-08-08,553 文件)确在基线之前(`git merge-base --is-ancestor 0814cac 33bc13b` 通过)。

## 2. 实跑复现(独立于 B4,全部一手输出)

1. **rc.1 实例**:`heimdall-rc1 init` 于 scratchpad(端口 26310-26312)→ `Heimdall initialized`;`backup create` 产出 manifest `schema_version:19, format_version:2` 的 `.hmbk`,与 B4 所述一致。
2. **同密钥、同归档、双二进制对照**(排除"B4 实跑构造有问题"的可能):`heimdall-rc1 backup verify` **通过**(证明密钥正确、归档完好);`halro-head backup verify --file rc1.hmbk --key-file backup.key` → **`read backup archive: backup authentication failed`**。断裂在版本边界,不在密钥或归档。
3. **空 rc.1 数据目录 + HEAD 启动**:`halro-head start` → 全部日志 **2 行**(1 行 host hardening + 1 行错误),错误为 **`master key does not authenticate the metadata store`**。master key 是 rc.1 init 刚生成的那把,完全正确。
4. **配置被拒**:HEAD 二进制读 rc.1 生成的 config.yaml → 3 个 `adjustment_*_micros_usd` 字段逐行报 `not found in type config.Admin`。与原文一致。(一处构造差异:B4 写 restore 报 auth failed;我的 restore 因先读本地 config 而在 config 解析处更早失败——verify 已独立证明 auth failed,该差异不影响结论。)

## 3. 原判遗漏的加重情节:一次失败的升级尝试就把 rc.1 目录变成两头不认

复现 §2.3 之后,用 **rc.1 自己的二进制**再跑 `doctor`,得到:

```
metadata schema version 27 does not match required version 19
```

即:HEAD 启动在 vault key-check **之前**已经把 bbolt 从 v19 迁到 v27 并提交(`boltstore.Open` 内迁移,`internal/store/bolt/store.go:1131-1209`;key-check 在其后,`runtime.go:933`),key-check 失败并不回滚已提交的迁移。后果:操作者拿 HEAD 试一次启动失败后,**退回 rc.1 二进制也打不开了**——"留在旧构建"这条 schema-20 拒绝路径承诺的退路,在 rename 边界上试过一次就消失。B4 的 F-2 没有记录这一点;它让"误导性错误"从"难排查"升级为"排查动作本身有破坏性"。这是维持 P1 而非下调的独立理由。

## 4. 证伪方向逐条清算

- **"公告写在别的文件里"**:失败。`CHANGELOG.md` 与 `docs/milestones/release-notes-v1.0.0.md` 中 `rename|heimdall` 零命中;全 docs/ 范围(排除 docs/review)对 `migration 24~27`、`schema 24~27`、`reset_capability` 零命中;两条错误文案(`authentication failed`、`does not authenticate`)在 docs/guides/、docs/runbooks/ 零命中——错误信息在任何地方都没有版本不兼容的解释。相反,`release-notes-v1.0.0.md:126-141` 的 "Installation and upgrade" 描述的是一条常规升级流程(停机→备份→换二进制),`CHANGELOG.md:149-152` rc.1 段自称记录"早期操作者需要采取的差异"——二者都在暗示连续性,与事实相反。
- **"域串其实兼容或有迁移"**:失败,见 §1.3。
- **"B4 实跑构造有问题"**:失败。双二进制对照(§2.2)证明失败恰好落在版本边界;仅发现一处不影响结论的 restore 时序差异(§2.4)。
- **"CHANGELOG 空提交是统计口径问题"**:失败。`git log --oneline 33bc13b..2cd24a7 -- CHANGELOG.md` 确为空;区间体量 `git diff --shortstat` = **193 文件、+22,648/-4,496**,"22.5k 行"属实。迁移 24(store.go:591)、25(:609)、26(:630)、27(:651)存在,reset_ 前缀恰两条(25/26),与 B4 的 F-5 自我修正一致。
- **"迁移其实有日志"**:失败。迁移循环(store.go:1131-1209)无任何 log/print 语句;实跑 v19→v27 迁移全程 0 行提示(§2.3 的 2 行日志里没有它),结构上支持 B4 "v23→v27 九行日志零提示"的观测。

## 5. 核心追问:"rc.1 用户"是否存在

- **二进制渠道:不存在。** `gh release list --repo akz142857/Halro` 为空——GitHub 上从未有任何 Release,一个都没有(与 B3 的 publish job 结论互证)。没有人下载过 rc.1 二进制,因为它从未存在。
- **源码渠道:无法证明不存在,且有非零信号。** 仓库 **public**(2026-07-31 建),`v1.0.0-rc.1` 标签已推到公网 origin(`git ls-remote --tags origin` 可见);旧名 `akz142857/Heimdall` 在 GitHub 重定向到现仓库,即 rc.1 的 module path 至今可解析、可构建。流量 API(`gh api repos/akz142857/Halro/traffic/clones`):近 14 天 **5,956 次 clone、498 个唯一来源**,打 rc.1 标签当天(08-07)701 次/50 唯一。其中大量必是模块代理与爬虫,0 star、0 fork 也说明没有可见的人类关注——但"有人从源码构建并部署过 rc.1"这一命题**无法被证伪**,只能说无证据支持。
- **对严重度的含义**:若 rc.1 用户群可证明为空,F-2 应降为 P3(修文案的卫生问题);因其只能压到"低概率、非零",B4 的 **P1/需裁决是正确档位**——不是 P0(无已知受害实例),也不是 P2/P3(一旦命中,是数据目录+备份双废、错误信息指向错误方向、且按 §3 排查动作本身还有破坏性)。裁决路径与 B4 给的一致:发布说明写明"rc.1 从未发布、与 1.0.0 不互通、必须重建实例"即可带着发;不写不能发。修复成本是一节文档,没有理由不修。

## 6. 附录

### 读过的文件

- HEAD:`internal/backup/crypto.go`(170-218)、`internal/vault/vault.go`(130-170)、`internal/app/init.go`(110-175)、`internal/app/runtime.go`(920-950)、`internal/store/bolt/store.go`(591-660、823-845、1131-1209)、`CHANGELOG.md`(90-160)、`docs/milestones/release-notes-v1.0.0.md`(95-150)
- rc.1 树(`git show` / `git archive` 展开):`go.mod`、`internal/backup/crypto.go`、`internal/vault/vault.go`、`cmd/heimdall/main.go`、`configs/config.example.yaml`
- 评审材料:`docs/review/260811/releases.1.0.0/role-prompts.md`(§1、§5-B4、§8)、`docs/review/260811/releases.1.0.0/findings/B4.md`

### 跑过的命令(关键项)

```
git log --oneline 33bc13b..2cd24a7 -- CHANGELOG.md          # 空
git diff --shortstat 33bc13b..2cd24a7                        # 193 files, +22648 -4496
git merge-base --is-ancestor 0814cac 33bc13b                 # rename 在基线前
git show v1.0.0-rc.1:internal/backup/crypto.go | grep backup:v1   # heimdall:backup:v1
grep -rn heimdall internal/ cmd/ --include='*.go'            # 0 命中(无兼容回退)
grep -rn "authentication failed|does not authenticate" docs/guides docs/runbooks  # 0 命中
gh release list --repo akz142857/Halro                       # 空(零 Release)
gh api repos/akz142857/Halro --jq '{private,visibility,forks,stars}'   # public, 0/0
gh api repos/akz142857/Halro/traffic/clones                  # 14 天 5956 clone / 498 uniques
go build -o $S/bin/halro-head ./cmd/halro                    # HEAD 二进制
(rc1-src) go build -o $S/bin/heimdall-rc1 ./cmd/heimdall     # rc.1 二进制
heimdall-rc1 init && backup create --output rc1.hmbk --key-file backup.key
heimdall-rc1 backup verify --file rc1.hmbk --key-file backup.key      # 通过(对照组)
halro-head   backup verify --file rc1.hmbk --key-file backup.key      # backup authentication failed
halro-head   start --config config-head.yaml   # 2 行日志,master key does not authenticate the metadata store
heimdall-rc1 doctor                            # 事后:schema 27 ≠ required 19(目录被单向迁移,§3)
```

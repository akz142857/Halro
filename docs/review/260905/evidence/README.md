# 可复现证据包

基线：`381743f6613607dc256828f4776b52af8bdd232c`，2026-09-05。本包仅保存评审专用小型合成 fixture、精选日志、实际退出状态及哈希索引，不是生产数据备份。没有复制 key 文件、cookie/session、credentials、配置、证书、运行数据库、加密备份或生产源码全文。`.go.txt` 不会被普通 `go test ./...` 纳入。

## 运行

从完整 Halro 仓库根目录运行，路径不依赖原作者的用户名或 `/private/tmp`：

```sh
python3 docs/review/260905/evidence/run_repros.py --list
python3 docs/review/260905/evidence/run_repros.py --check
python3 docs/review/260905/evidence/run_repros.py --dry-run all
python3 docs/review/260905/evidence/run_repros.py --run all
# 单项示例
python3 docs/review/260905/evidence/run_repros.py --run SEC-02
python3 docs/review/260905/evidence/run_repros.py --run SEC-04-C2
```

`all` 串行执行下表九个选择器，不运行全量 Go/web 门禁、fuzz 或 load benchmark。每项只有一个虚拟测试文件，overlay JSON 在系统临时目录构造并清理；绝不替换现有源码/测试。合成数据由现有 `t.TempDir()` fixture 创建并清理，Go 正常使用构建缓存。runner 校验源码 SHA-256、必要依赖文件和虚拟路径不存在；缺少完整仓库测试 helper 会明确拒绝或由编译器报告，不能只拷贝此目录独立运行。

需要 Python 3.8+、Git、满足 `go.mod` 的 Go（本次为 1.26.6）以及已经准备好的模块缓存；race 两项还需平台支持的 C 工具链。runner 使用 `-mod=readonly`、`-count=1`、精确 `-run`、90秒测试超时，清除继承的 GOFLAGS、关闭 workspace，`GOPROXY=off` 禁止模块下载；允许已缓存的 Go 工具链自动选择，保留官方 checksum 验证。缺模块/工具链需另行准备，不会自动安装工具。checksum 缓存缺失可能需要网络验证；所有案例的 Provider/KMS/通知均为本地 fixture，不会调用真实 Provider。

默认检查 HEAD 等于基线；修复后复核可显式添加 `--allow-different-sha`。HEAD 相同不保证工作区无源码变化，应自行检查 `git diff`。fixture 是基线刻画测试：多数 **exit 0 表示成功观察到缺陷**，不是不变量成立；修复后可能按预期失败，不能不经判断直接转为 CI 回归。runner 返回任一失败则整体 exit 1，不通过管道尾部推断结果。输出到 stdout，不自动归档未来输出；未来日志入库前仍需脱敏检查。

## 用例与覆盖

| runner ID | 证据及断言 | 边界 / 报告 |
| --- | --- | --- |
| SEC-01-file | 真实 file Master Key 轮换成功后 capture/object 新 key 解密失败，旧 key 仍可读 | P1；合成对象未注册成公开资源；[裁决](../roles/security-rotation-adjudication.md) |
| SEC-01-kms | fake KMS 更换 DEK 重现；同 DEK rewrap 控制通过 | P1；无真实 AWS；同上 |
| SEC-02 | logout 删除后，阻塞的 refresh 重插；新 manager/新 HTTP 请求接受旧 token；generation/absolute expiry 控制有效 | P1；`-race`；[裁决](../roles/security-session-adjudication.md) |
| PROV-01 | metered snapshot、price pin、正预留下坏 HTTP200 重试两次并记0；400和ambiguity反事实控制 | P1；内存 RoundTripper 拦截全部请求，不证明真实计费；[裁决](../roles/provider-adjudication.md) |
| SUM-01 | checkpoint 取出期间请求1→0→1、费用90→0→90 | P2；暂停边界，不是永久账务丢失；[裁决](../roles/deadman-adjudication.md) |
| REL-01 | 真实 Run worker 在持久化前发送，durable 0/41 两种情况重启模型重用ID | P2；`-race`；不是 kill/掉电实验；同上 |
| PROV-02 | CR-only完整事件被扣到EOF才交付 | P2；150ms有界观察，不是吞吐/全协议证明；[报告](../roles/provider.md) |
| SEC-03 | 四个MFA管理路径错因子7×401无失败审计；错密码5×401后429 | P2；合成固定TOTP输入，非真实secret导出；[裁决](../roles/security-secondary-adjudication.md) |
| SEC-04-C2 | 精确TTL/过期正文200；read_only按现契约可读；审计失败503无正文、Purge后404 | SEC-04 P2；C2越权解释被证伪，不是漏洞；同上 |

当前九项所需 review fixture 均已找到。`cases.json` 记录包、选择器、race标志、依赖路径、原始与归档源码哈希。file轮换和CR案例从原始混合测试中抽取并收窄imports，其余只gofmt；没有复制生产实现。`SEC-03`/`SEC-04-C2` 共享一个源码文件但分别执行。旧日志的文件名/行号属于原始 overlay，不能直接当归档后行号。

## 日志、退出码和来源

- `logs/`：人工检查的小型原始关键日志；只含测试名、合成ID、错误分类、计数、耗时。PROV-02历史日志同时含早期PROV-01例子，P1裁决应优先使用独立PROV-01日志。固定虚构 bearer 字符串、MFA测试字节和合成正文仅存在于 fixture 源码，不是从实际session/credentials文件导出。
- `log-index.json`：原始文件SHA-256、大小、归档位置、转换说明与退出码依据。`SHA256SUMS` 对本次归档文件提供独立完整性校验，**不覆盖未来主持的 runtime 子目录**。
- `gate-results.json`：主持已完成 Go、web、vet、concurrency/admission race、fuzz 的真实进程结果；本次归档没有重跑这些门禁。`logs/web-build.log` 去掉冗长asset大小表，checkout路径替换为`<repo>`。原始完整文件哈希仍在索引。
- `logs/npm-audit.json` / `logs/npm-audit-result.json`：主持在 `web` 执行官方 registry 的只读 `npm audit --json --registry=https://registry.npmjs.org`，实际 exit 0（2.968s），`metadata.vulnerabilities.total=0`、`metadata.dependencies.total=199`。仅适用于本轮固定依赖与2026-09-05的查询响应，不是永久无漏洞保证；未运行 audit fix 或修改依赖。gate索引另记当前lockfile指纹。
- SDK Python 的 `logs/sdk-python.log` 是 **0字节**。成功依据是主持观察的进程实际 **exit 0**，在 [runtime-evidence.md](../runtime-evidence.md) 第2节记录；空stdout自身不能证明成功。本包未找到独立SDK机器可读退出码receipt，因此明确标为主持进程记录引用，不冒充归档者独立重跑。Go/Node同样不因日志内容单独推断退出码。
- `upgrade-seeded-dns-results.json`：逐步进程退出/HTTP状态、只读命令未改文件、Ledger sequence8→8；临时路径占位化。没有归档config、key、Ledger或二进制。schema拒绝步骤exit1是预期结果，不是整体演练失败。完整说明见 [runtime证据](../runtime-evidence.md)。

升级装配、SDK服务/依赖安装、原始mock运行数据不在此复现runner中；不能凭结果JSON重建完整升级/SDK运行。fuzz只保存限时运行命令/退出/计数，无失败crash样本，新interesting corpus未归档，不能逐样本重放。未归档其他roles全部源码、配置省略/数学补充fixture或大规模安装日志；它们仍是相应报告的覆盖/限制，不假称本包全部可执行。

## 打包验证与权限交接

见 `packaging-validation.json`：归档后的定向运行及初次工具链/抽取错误分别记录。最初强制local使1.26.2不满足go.mod；第二次关闭checksum验证阻止缓存工具链使用；恢复验证后file案例因抽取遗漏bytes import编译失败，已仅修复归档fixture imports。这些是打包/环境问题，不列为生产缺陷，成功项不重复执行。

本次仅修改 `docs/review/260905/evidence/`，不改roles或主报告，不安排其他作者并发修改。**`evidence/runtime/` 自现在起由主持独占写入，用于后续 soak/backup 等结果；本归档者不创建、覆盖或索引该子目录。** 本次结束后，主持可接管其余evidence文件维护；如后续修改，更新相应哈希索引。旧的历史结果不可覆写为修复后的新结果。

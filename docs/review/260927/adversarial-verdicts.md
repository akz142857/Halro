# 严重 Finding 对抗裁决

基线：`0c4a270d7aae2b3d4562c35cd60ff14907278897`。

| ID | 初始命题 | 反证尝试 | 裁决 |
| --- | --- | --- | --- |
| R-001 | `in_flight` 未确认可导致同 key 再次调用 Provider | 核对 Reservation/Attempt confirmation、store journal class、promotion confirmed-prefix apply、reclaim 分支；现有防御只能保证 attempt 已记账，不能把“已发给 Provider”带到新 Primary | **CONFIRMED P0** |
| R-002 | 资源接口绕过 inference scope | 核对 HTTP Guard、`resourcePrincipal`、Project/model/source/budget gates；未找到其它 scope gate，且 adapter 调用从该 principal 直接可达 | **CONFIRMED P1** |
| R-003 | planned stepdown 后旧 Primary 仍可发起 Provider I/O | 核对 Freeze、readiness、HTTP shutdown、AttemptStarted confirmation；Freeze 不等待 active handler，Shutdown 不取消其 context | **CONFIRMED P1** |
| R-004 | logout/MFA 状态在 promotion 后复活 | 核对 journal class、confirmation classifier、promotion invalidation、Audit ordering；未找到 promotion 时整体失效，Audit 也不提供同步确认 | **CONFIRMED P1** |

R-001 由路由/协议与 HA/数据两个角色独立得到同一时序；主 Reviewer 逐行核对了 mark、confirmation classifier、`WaitConfirmed` 和 Provider 调用顺序。R-003/R-004 由 HA 角色提出，主 Reviewer复核了 server shutdown、store mutation 和 confirmation 路径。R-002 由协议角色提出，主 Reviewer对照普通推理与 Models 的 scope gate 复核。

没有执行真实计费 Provider 调用，也没有为“证明漏洞”破坏仓库代码。动态 fault-injection 用例被列为每项的关闭测试；在这些 oracle 加入前，现有 green suite 不能证伪上述源码时序。

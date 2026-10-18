# V-010 — P2：主动探测打开的 deployment 挂起在半开探测持续失败时既不延长也不刷新，显示的结束时间早已过去，运维无法清除

- 基线：冻结 `main` `2e749db1`
- 段/功能：A 段 / F01 RouteGate（#324 引入，不在 v0.8.5）；F02 路由挂起
- 分类：【问题】，真实二进制现象（E05）+ 确定性单元复现
- 发现方式：E05 长跑中意外触发，260927 与本轮阶段 2–3 都没有读到

## 现象（E05，真实二进制）

E05 的 Provider 指向不可解析的 `https://halro-smoke.invalid`。前 60 分钟约 53 万个请求全部 502，没有任何挂起。第 60 分钟（08:07:47Z）每小时一次的主动探测失败，deployment 被挂起 30 s：

```
scope_kind=deployment reason=unclassified provider_code=connect
observed_at=2026-10-18T08:07:47Z until=2026-10-18T08:08:17Z indefinite=false clearable=false
```

25 分钟后列表**仍是同一条、同样的 `until`**，而约 97% 的请求返回 503 `all_candidates_suspended`，其余是半开放行的探测请求（`halro_route_probe_admitted_total{outcome="still_failing"}` 7,025）。`halro_route_suspension_transitions_total` 只有 1。

## 确定性复现（冻结 SHA 的 scratch worktree，一次性测试，未提交）

`ObserveProbe` 以 connect 失败挂起 `dep_1`；窗口过后 20 s 内每 100 ms 一个请求，半开放行的都以 `Observation{Class: ErrorConnect}` 结束：

```
admitted=5 refused=195
deployment/dep_1   until=12:00:30Z (now=12:00:51Z) observed=12:00:00Z
provider/provider_1 until=12:01:01Z                 observed=12:00:31Z
```

原因：半开租约的 `Done` 把失败交给 `observeReporting`，而 `policyFor` 把无状态码的 connect/timeout 失败归到 **provider** scope（`internal/routegate/gate.go:136-149`）。被探测打开的 **deployment** scope 只在 `Done` 里归还了半开名额（`releaseClaims`），没有被再观察：不加倍窗口、不更新证据、不延长 `suspendedUntil`。`suspended()` 只看 `suspendedUntil` 非零（`:75`），于是这条挂起一直有效，直到某个请求成功（`ObserveSuccess`）或下一次主动探测成功（一小时后）。`observeLocked` 的注释写的是"A probe that failed doubles the window it just served"（`:235`），这条路径上没有发生。

单元复现里 provider scope 在 5 次失败后也被挂起；E05 真实二进制里没有出现第二次挂起（transitions 只有 1）。两者为何不同**本轮没有查清**，不在此推断。

## 影响

上游真实故障时：

- 控制台与 Admin API 显示的挂起"结束时间"是几十分钟甚至一小时前，证据时间也停在第一次失败；运维读到的是"挂起早已结束"，而流量正被拒绝；
- 这条挂起来自探测、只在内存（`clearable=false`），控制台不给清除按钮，`halro route clear-suspension` 也找不到持久行——除重启外无法手动解除；
- 半开探测没有退避：窗口不加倍，名额一释放就放行下一个，上游恢复前持续以单并发打到上游。

上游恢复后，第一个成功的半开请求会清除挂起，所以不会永久卡住；账务不受影响。

## 建议

半开租约失败时，对它占用的每个 scope 都按该 scope 的策略再观察一次（至少刷新 `suspendedUntil` 与证据、按注释加倍窗口），而不只是观察 `policyFor` 选中的那一个；补一个"探测打开的挂起在半开连续失败时窗口加倍、`until` 前移"的测试。

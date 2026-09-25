# 诊断结论：Halro 用自己的数字回答的四个问题

> 面向运营者。只读，不联网，不调用任何模型。
> 相关实现：`internal/advisor/`、`internal/app/doctor_advisor.go`、
> `internal/app/admin_advisor.go`、`web/src/pages/AdvisorFindingsPanel.tsx`。

## 它是什么，为什么会有

一次真实事故里，三天内三次请求以 `No secure connection could be established`
失败，附带的建议是去查 DNS、TLS 和出网代理。网络没有任何问题。把原因找出来
需要把一行日志、一个配置文件和一份源码并排读一遍，而结论全部是算术：

| 结论 | 需要的运算 |
| --- | --- |
| `latency_millis: 60021` ≈ `gateway.attempt_response_header_timeout: 1m0s`，所以这是自己的超时 | 一次减法 |
| `retry.max_attempts_per_target` × `gateway.max_total_attempts`，所以第三个备选永远走不到 | 一次乘法 |
| `attempt_response_header_timeout` × 2 ≥ `route_total_timeout`，所以配置的重试放不下 | 一次乘法 |
| `server.shutdown_timeout` < `route_total_timeout` | **已经是硬校验**（`internal/config/config.go`） |

前三条和第四条是同一类陈述。第四条被强制执行，前三条此前不由任何东西说出来。

**这些都不需要模型，只需要把 Halro 自己的数字放在一起。** 为什么不让模型来讲，
以及在什么条件下才值得重新考虑，记在 issue #346 里。

## 在哪里看

- **离线**：`./bin/halro doctor --config config.yaml`，输出 JSON 的 `findings` 字段。
  只读，可以对着真实数据目录跑。
- **在线**：控制台 → 设置与状态 → 诊断 → 「诊断结论」卡片，30 秒刷新一次。

两个视图跑的是同一套规则。区别在于离线视图没有进程可问，所以与运行时状态相关的
两条规则会报 **未检查**，而不是报正常。

## 一条结论由什么组成

```json
{
  "rule": "attempt_budget_reaches_fanout",
  "status": "warn",
  "evidence": [
    { "name": "gateway.max_total_attempts", "value": "3" },
    { "name": "retry.max_attempts_per_target", "value": "2" },
    { "name": "candidates_the_budget_reaches", "value": "2" },
    { "name": "widest_fan_out", "value": "3" },
    { "name": "widest_fan_out_public_model", "value": "chat-fast" }
  ],
  "comparison": "ceil(3 / 2) = 2 < 3",
  "consequence": "\"chat-fast\" has 3 candidates and the attempt budget reaches 2 of them; the rest are configured and never called"
}
```

**结论带的是产生它的证据，而不是一句关于证据的话。** 两个数字、比较、以及一句
后果——顺序如此，因为运营者可以拿 `evidence` 里的键去 `config.yaml` 里核对，
而一句话只能选择相信。

三种状态，彼此不可合并：

| 状态 | 含义 |
| --- | --- |
| `ok` | 规则跑了，没有发现它要找的组合 |
| `warn` | 规则跑了，发现了。**不是错误**：每条规则描述的都是 Halro 接受并运行的配置 |
| `unknown` | 规则要读的输入拿不到（离线视图没有进程可问） |

**跑出来没问题的规则也保留自己那一行。** 只显示问题的面板无法区分「查过了，没事」
和「根本没查」，而后者才是更值得知道的状态。

## 五条规则

### 1 · 单次尝试的超时有机会触发吗

`gateway.attempt_response_header_timeout` ≥ `gateway.route_total_timeout` → warn。

前者限制一次尝试等待响应头的时间，后者限制整条请求。把前者配成大于等于后者，它
就永远不会是先触发的那个——整条请求的截止时间总是先到。于是一个卡住的上游花掉
整次请求的预算，下一个备选一点都拿不到。

**今天没有任何校验检查这一对**，这样配置的实例启动完全正常。

### 2 · 配置的重试放得进请求预算吗

`attempt_response_header_timeout` × `retry.max_attempts_per_target` >
`route_total_timeout` → warn。

针对同一个目标的重试要和其它一切共用整条请求的预算。放不下时，对一个慢目标的
第二次尝试会被请求截止时间中途切断，这个重试就是一段永远不会执行的配置。

规则写成按目标计，而不是按 `max_total_attempts` 计，是刻意的：最坏情况下总次数
routinely 超预算（出厂默认的四次 1m0s 尝试装不进 2m0s），但那需要每次尝试都卡满
自己的超时，这不是失败的常见形状。对它告警会在全新安装上就亮起，然后运营者就学会
忽略这个面板。那个数字作为**证据**列出来，放在备选数旁边，在那里它才回答问题。

### 3 · 配置的每个备选都会被调用吗

`ceil(max_total_attempts / max_attempts_per_target)` < 最宽别名的备选数 → warn。

候选是按公开别名逐个走的，所以比较对象是**某一个别名后面最多有几个备选**，不是
路由总数：一百个别名各挂一个备选，预算一就够了。

超出部分的路由配置了、启用了、健康，但永远不会被调用。在线视图数的是注册表里的
目标（那才是请求真正会走的清单），离线视图数的是路由表里指向启用中部署的路由。

### 4 · 现在有什么被挡在路由之外

准入门（`internal/routegate`）当前挂起的作用域，连同它们的原因和上游状态码。

这个列表控制台里本来就有。这条规则的作用是把它和上面的算术**放在同一次阅读里**，
因为两者合起来才是一个问题的答案。没有带原因的挂起是可用性策略做的——它统计连续的
连接失败、超时和 5xx，不区分它们——所以 `gateway.attempt_response_header_timeout`
会被列在同一行的证据里：那是先该看的数字。

### 5 · 上游的拒绝被读懂了吗

分类失败的拒绝次数，以及它们当时带的 HTTP 状态码。

读懂的拒绝会驱动路由：订阅额度耗尽和限流走的是不同的时钟。读不懂的落到可用性策略，
它统计连续失败并短暂挂起——这对「上游一时不稳」是对的处置，对「账户没钱了」是错的。
`{reason="unclassified", provider_status="402"}` 这一对此前只存在于一条 Prometheus
时间序列里，运营者得先在抓取它才看得到。

## 它不做什么

- **不改任何东西。** 结论只陈述，决定由运营者做。没有「一键修复」。
- **不影响 `halro doctor` 的退出码。** 一条结论描述的是 Halro 接受并运行的配置；
  一项检查描述的是实例是否完好。把前者折进后者，会让一条运营者有意选择的超时
  搭配使 `doctor` 非零退出，所有据此判断的部署脚本都会停下。
- **不读任何调用方内容。** 输入只有配置值、计数、枚举，以及运营者自己配置的标识符。
  没有 prompt，没有响应体，没有凭据，没有源地址——这也正是它可以渲染在浏览器里的原因。
- **不跨实例、不跨 Project 分析。**

## 顺带修掉的观测缺口

建规则会暴露观测缺口，而不是把它盖住。第一个撞上的：延迟直方图的桶边界原本是

```
10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 120000   // ms
```

**30s 和 120s 之间只有一个桶。** 而这恰好是「自己的截止时间在切请求」时请求所处的
区间：出厂的 `attempt_response_header_timeout` 是 1m0s，`route_total_timeout` 是
2m0s。「90 秒对这个部署够吗」在这个形状下答不出来——55s 和 90s 落在同一个桶里，
事故排查时只能用肉眼去读控制台的逐次尝试延迟列。

现在加了 45000、60000、90000 三个边界。**一条规则需要一个指标表达不出来的数字，
本身就是一条关于指标的结论。**

### 这会影响已有实例

直方图的含义变了，所以两个带直方图的派生格式都跟着提了版本：
`usage.checkpointVersion` 14 → 15，`domain.RollupVersion` 1 → 2。两者都是 Ledger
的派生物，旧版本会被拒绝并从 Ledger 重放重建——不需要人工操作，但首次启动时会多
一次重放。Prometheus 那边保留旧的 `le` 序列并开始一组新的，这是重新切分直方图的
常规后果。

# V-014 — P2：时钟类计价隔离没有任何解除途径

- 基线：**v0.8.5 已存在**，冻结 `main` 与当前 `main` 相同
- 段/功能：计价隔离（`internal/store/bolt/store_pricing.go` `confirmRestoredPricing`）
- 分类：【问题】，代码确认
- 发现方式：处理 V-013 时，为 E05d 解除 E05b 的隔离而查找途径
- 状态：未修复，需要 owner 设计决定

## 现象

唯一把 `Quarantined` 置回 `false` 的代码是 `confirmRestoredPricing`（Admin 的恢复确认动作），它只接受 `restored_` 前缀的原因：

```go
if !high.Quarantined || !strings.HasPrefix(high.QuarantineReason, "restored_") {
    return ErrRevisionConflict
}
```

`wall_clock_rollback` 与 `wall_clock_forward_jump` 没有 Admin 动作、没有 CLI 命令可以解除。operator-guide 只说隔离会阻塞 deployment，没有说如何解除。

## 影响

V-013 或真实的时钟跳变一旦触发，该 deployment 永久停止服务，只能删除重建（连同其上的价格时间线与路由引用）。V-013 修复后误判不再发生，但真实时钟跳变仍会走到这里。

E05d 为继续测试，在 Halro 停机时用一次性离线工具改写了测试数据中的高水位记录（`quarantined` 去掉、revision +1），前后内容记录在 E05d 日志。这不是产品路径。

## 建议

增加一个解除时钟隔离的 Admin 动作：要求近期重新认证、写审计、记录操作者确认的当前时间；解除时以当前时间重设高水位，并清空内存锚点。

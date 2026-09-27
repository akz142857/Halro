# R-014 — P2 CONFIRMED：固定 key limiter 不覆盖 scope/CIDR 拒绝

Models discovery 的顺序是 Authenticate → inference/discovery scope → Project source → key limiter（`internal/gateway/models.go:77-100`）。当可配置 source limiter 被关闭时，持有有效但缺 scope、或来源不允许的 key 可以持续触发 403，却永远不会进入不可关闭的 600 RPM key ceiling。

这不触发 Provider I/O，也没有信息泄露；认证/scope 检查成本相对低，所以按 P2 记录。但它与固定 limiter 对 authenticated local reads 提供不可关闭上限的产品描述不一致，且现有 key-rate tests 只覆盖授权请求。

关闭条件：明确契约。如果固定 ceiling 覆盖所有已认证请求，应在 scope/source 判定前收费并补充 403→429 测试；若这些 cheap denials 有意例外，则同步 contract、注释和容量/DoS 理由。

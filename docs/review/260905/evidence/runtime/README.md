# Runtime 脱敏记录

本目录由主持维护。全部请求和身份来自隔离合成数据，不含实际Key、cookie、证书、数据库或备份。baseline记录环境；三个inventory记录源码盘点；lifecycle与browser-project-calls记录实际API结果，浏览器动作与UI状态见 [运行报告](../../runtime-evidence.md)；upgrade两组记录真实旧/新二进制结果。

latency-probe是100个客户端完整响应耗时（50 unary、50 SSE），并发4；不是首token时间或生产SLO。backup-live-lock记录运行中拒绝离线备份。soak-command-valid对应唯一有效30分钟样本；soak-first-attempt记录早先fixture缺少模型详情健康路由的失效尝试，不混入有效数据。

完整Runtime使用报告声明的测试overlay，不据此认证生产出站TLS/SSRF。JSON不是可导入的生产配置，不能凭结果文件一键重建所有进程和浏览器状态。可移植缺陷复现见父目录runner；运行步骤、现场观察与局限见运行报告。最终SHA256SUMS仅覆盖本目录当时的普通文件，不含自身。

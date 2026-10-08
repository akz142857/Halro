# 认证范围删除与 Metadata 桶范围

本契约补齐 I-11 完整 Metadata 帧恢复的结构性删除依赖；未接默认 Runtime 或完整 native reducer。

`Transaction.DeleteRange(lo, hi)` 在单个 callback-scoped COW transaction 中删除 `[lo, hi)`；空端点表示无界。成功的返回数量来自认证树已提交的行计数。一个范围占用一次 mutation，并计入既有累计输入字节/读页/append ceiling、共享 64MiB Update 工作费用。非法范围、费用不足、取消、读页/写次数超限以及被忽略的方法错误都不能提交部分根；逃逸 transaction 无法继续调用。

实际根先经 `query.load` 检查摘要、结构与 count。部分覆盖的边界子节点必须读取并认证；完整覆盖的子树使用已认证父节点的 bounds/count/ref 摘除，不枚举其行或读 value。固定 fanout/height 的边界路径重写保留子树级别，只折叠最终 unary root，实际 count 等于原 count 减去移除 count。新节点读取使用当前实际已追加 extent，不能沿用旧文件上限。

该操作证明的是按已承诺键集合执行授权删除，不证明被丢弃的物理 value bytes 完整。它不审计完整删除子树内的后代文件；旧 pin 继续保留旧认证引用，任何实际读取的损坏都会报错并锁存 owner。保留子树的 reference/digest 未改变，未来读取仍需认证。不能把成功范围删除作为历史审计结果，也不能借此绕过调用者对 native 源、范围、角色和允许前缀的独立认证。

`metadatastate.BucketRange(path)` 返回 codec1 桶 marker 与全部后代的有限半开范围。组件终止编码为 `00 00`，名字中的零编码为 `00 ff`；取最终组件终止字节的后继得到 upper bound，所以二进制名字和前缀相近的兄弟桶不会混入。两个 bounds 自有 bytes，不引用输入或彼此。调用者必须先证明目标 bucket marker 存在，并维持 bbolt 的 value/bucket 同名互斥；该 helper 不认证 native op，也不授予删除某个业务范围的权限。

内容发布继续由既有 `Store.Update` 同步 pages、写入/同步 staging root、原子替换 root 和同步目录。删除、同帧重建和其他 point mutations 共享一个 candidate；旧 pin 保持旧内容。callback 失败时不发布部分根；若已追加边界节点，owner 停止查询，按独立旧 native cut 重开时修去未发布尾部。

这是逻辑结构删除，不物理回收 pack 的旧页，也不释放其持久 Disk 费用。旧 pin 的关闭、空间重建/compact、实际物理删除与 owner 接管仍必须独立完成。

本地证据包含 10000 行删除/重建、独立 map 的混合 churn、真实 bbolt 的 3005 行桶删除差分、二进制兄弟桶/嵌套空桶/sequence、旧 pin、真实关闭与重开、损坏边界和累计门禁。计数/读页/节点/append 的断言证明这些 fixture 的边界工作，不能替代全键宽度/完整规模的 Linux RSS 和性能接受。[验证证据](../verification/performance/2026-10-04-metadata-range-delete/README.md)。

后续[完整四类帧归约、bookkeeping 同帧推进及 1024/64 原子接受](authenticated-metadata-frame-delta.md)已接显式库接口。format1→2 的生产迁移/兼容、trim cut 状态根/resume、跨进程选择/费用/角色、完整 Seal 和默认 Runtime 仍未完成。

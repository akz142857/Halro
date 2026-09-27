# R-006 — P2 CONFIRMED：配置迁移的版本和文件边界不够严格

## 版本解析

`declaredVersion` 使用 `fmt.Sscanf(value.Value, "%d", &version)`，不检查输入是否被完整消费（`internal/config/retirement.go:299-313`）。因此 `version: 1junk` 会被解释成 v1 并可能被重写为当前版本，而不是按“未知 shape 不猜测”的声明 fail closed。

## 文件身份与耐久性

`--write` 用 `os.Stat` 读取 mode bits，backup 用 `os.WriteFile`，新文件经同目录 temp + chmod + rename 替换（`cmd/halro/config_migrate.go:52-96`）。这不会保留 owner/group、ACL、xattr；若输入路径是 symlink，rename 会替换 symlink 本身而不是更新其目标。backup、staging file 和父目录也没有 fsync，源码明确把这一点作为当前选择。

这些问题不构成已证明的部分写成功，但会破坏配置管理路径身份，且断电后的 backup/new-file 耐久性没有保证，按 P2 记录。

关闭条件：严格解析完整 version scalar；明确并测试 symlink 策略与 owner/group/权限策略；若命令继续承诺原子恢复，则同步 file 和 parent directory，并注入 write/rename/fsync 失败测试。

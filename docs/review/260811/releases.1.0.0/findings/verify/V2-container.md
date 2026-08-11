# V2 · 对抗验证：B1-01「容器 / K8s / systemd 部署布局全部无法启动」

- 角色：§8 对抗验证（任务是**证伪**，默认原判为错）
- 被验证对象：`docs/review/260811/releases.1.0.0/findings/B1.md` §5 B1-01（本轮唯一 P0）
- 评审 HEAD：`2cd24a76a569fe53f878c1ab1be31441f4c008e0`
- 实跑时间：2026-08-11 19:09 ~ 19:13 (+0800)，macOS 15.6 / OrbStack Docker 29.4.0 / buildx 0.33.0
- 所用模型：**Opus 5（claude-opus-5[1m]）**
- 拒答 / 空响应：**无**
- 仓库改动：**零**。所有改造过的配置都在 scratchpad 下（`.../scratchpad/v2/`），仓库工作树全程只读。

```
$ git status --porcelain
?? docs/review/260811/releases.1.0.0/
```

（仅评审目录未跟踪，与 B1 的基线一致；无任何仓库文件改动。）

---

## 1. 裁决：**PARTIAL**

| B1-01 的子主张 | 裁决 | 依据 |
|---|---|---|
| 发布锁建在 `filepath.Dir(dataDir)`，因此 `data_dir` 的父目录必须可写 | **CONFIRMED** | `internal/store/lock/lock_unix.go:62-65` + 本地实跑落盘位置 |
| 照 `operator-guide.md:489-497` 原文 `docker run`，100% 起不来 | **CONFIRMED（逐字复现，含同一 hash）** | 实证 A |
| `backup-restore.md:169-197` / `README.md:81-83` 写的是正确布局，与 operator-guide 直接矛盾 | **CONFIRMED** | 三处原文对照 |
| CI 的 `container` job 只跑 `version` + `image inspect`，从不挂卷启动 | **CONFIRMED** | `.github/workflows/ci.yml:141-155`、`release.yml:192-201` |
| **「四份部署工件都把数据目录设成挂载点本身」** | **REFUTED** | 只有 **一份**（`operator-guide.md:497`）真的设定了 `data_dir`。Dockerfile / systemd unit / k8s manifest **都不设定 `data_dir`** |
| **Dockerfile「镜像开箱只支持这个坏布局」** | **REFUTED（且事实相反）** | 镜像的 `WORKDIR /var/lib/halro` + 仓库自带 `configs/config.example.yaml:66` 的相对路径 `./data` 恰好解析成**正确布局**，实跑 `Halro initialized`（实证 F） |
| **k8s manifest 因 `readOnlyRootFilesystem` + PVC 挂 `/var/lib/halro` 而必然失败** | **REFUTED（作为独立工件）** | 同一 manifest 形状在 `data_dir` 取挂载点子目录时完全正常，且能起服务、healthy（实证 D、E） |
| **systemd unit 因 `ProtectSystem=strict` + `ReadWritePaths=/var/lib/halro` 而必然失败** | **REFUTED（作为独立工件）· 未实跑** | 没有任何文档给裸机 systemd 规定 `data_dir=/var/lib/halro`；`operator-guide:497` 那句自我限定为 "**Container** configuration"。取子目录时 `ReadWritePaths=/var/lib/halro` 正好是对的 |

**一句话结论：缺陷真实、可 100% 复现，但爆炸半径被高估了 4 倍——它是 `operator-guide.md:497` 一句话的错误，不是四份工件的同源缺陷；镜像本身默认就是对的。**

---

## 2. 机制核对（读码）

`internal/store/lock/lock_unix.go:62-65`：

```go
func publicationLockPath(dataDir string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(dataDir)))
	return filepath.Join(filepath.Dir(dataDir), fmt.Sprintf(".halro-publication-%x.lock", digest[:8]))
}
```

调用链（两条都经过它，无旁路）：

- `lock.Acquire(dataDir)` → `:21` `acquireFile(publicationLockPath(dataDir), "publication")` → `:71` `os.OpenFile(path, O_CREATE|O_RDWR, 0600)`
- `lock.AcquireInitialization(dataDir)` → `:59` 同一个 `publicationLockPath`；被 `internal/app/kms_master_key.go:414` 使用（即 KMS key_slots 模式——正是 systemd/k8s 两份 manifest 的模式——也走同一把锁）

`acquireFile:68` 的 `os.MkdirAll(filepath.Dir(path), 0o700)` 对已存在目录直接返回 nil（不改权限），所以拦不住这条路径。`:71` 的 `O_CREATE` 需要父目录的**写权限**。没有任何 fallback、没有降级到 `dataDir` 内部。**机制主张成立。**

本机落盘验证（scratchpad，`data_dir = <parent>/data`）：

```
$ .../v2/halro init --config .../v2/local/cfg.yaml
Halro initialized
$ ls -la .../v2/local/parent
-rw-------  .halro-publication-efb60a6a8547b79b.lock     <- 锁在 data_dir 的父目录
drwx------  data
-rw-------  master.key
```

---

## 3. 实跑证据（Docker，全部真跑）

镜像：`docker build -t halro-v2:test .`（成功，`sha256:f2a6e0f0cc1a...`）。
镜像内 `/var/lib` 的属主（`docker export | tar -tv`）：

```
drwxr-xr-x  0 0      0        var/lib/            <- root:root 0755
drwxr-xr-x  0 65532  65532    var/lib/halro/      <- 只有这一层被 chown
```

即 `Dockerfile:27` 的 `COPY --chown=65532:65532` 只覆盖 `/var/lib/halro` 自身，`/var/lib` 仍是 root:root 0755。**「distroless 里 /var/lib 本就可写」这条证伪方向被排除。**

### 实证 A — `operator-guide.md:489-495` 逐字执行

配置按 `:497` 要求：`storage.data_dir: /var/lib/halro`，`master_key.file: /run/secrets/halro-master.key`。

```
$ docker run --rm --user 65532:65532 \
    -v "$PWD/config.yaml:/etc/halro/config.yaml:ro" \
    -v "$PWD/master.key:/run/secrets/halro-master.key:ro" \
    -v halro-data-v2:/var/lib/halro \
    -p 18080:8080 -p 18081:8081 halro-v2:test
{"time":"2026-08-11T11:09:58.429300958Z","level":"INFO","msg":"runtime host hardening applied","core_dumps_disabled":true,"process_dump_disabled":true,"managed_heap_dontdump":false}
halro: open publication lock: open /var/lib/.halro-publication-fdff7c09c3790d29.lock: permission denied
```

**与 B1 报告的报错逐字一致，连 digest `fdff7c09c3790d29` 都相同**（因为它是 `data_dir` 字符串的 SHA-256 前 8 字节，与环境无关）。独立复现成立。

### 实证 B — 排除「B1 自己的 volume 属主设错」这一证伪方向

`-v halro-data-v2:...` 是 Docker 具名卷首次使用，Docker 会把镜像层内容（含 65532 属主）复制进卷，属主正确。为彻底排除，再跑一次**完全不挂卷**：

```
$ docker run --rm --user 65532:65532 -v <config> -v <key> halro-v2:test init --config /etc/halro/config.yaml
halro: open publication lock: open /var/lib/.halro-publication-fdff7c09c3790d29.lock: permission denied
```

失败与卷无关，纯粹是 `/var/lib` 的属主。**该证伪方向被排除。**

### 实证 C — 以 root 运行（低成本绕过检验）

```
$ docker run --rm --user 0:0 ... -v hv-root:/var/lib/halro halro-v2:test init --config /etc/halro/config.yaml
halro: master key already exists
```

root **能**越过发布锁（错误已推进到下一阶段的 master.key 检查，即 B1-02 那条）。所以「以 root 跑」在裸 Docker 下确实是一条绕过——但它同时废掉镜像的 nonroot 设计，且在 k8s 上被 `runAsNonRoot: true`（`deploy/kubernetes/halro-aws-kms.yaml:26`）直接禁止。**不是一条可接受的绕过。**

### 实证 D — k8s 形状（`readOnlyRootFilesystem` 等价）：`data_dir` = 挂载点

`--read-only` + `--tmpfs /tmp` + PVC 等价的具名卷挂 `/var/lib/halro`，uid 65532：

```
$ docker run --rm --read-only --user 65532:65532 --tmpfs /tmp -v <config data_dir=/var/lib/halro> -v hv-k1:/var/lib/halro halro-v2:test init --config /etc/halro/config.yaml
halro: open publication lock: open /var/lib/.halro-publication-fdff7c09c3790d29.lock: read-only file system
```

注意错误从 `permission denied` 变成 `read-only file system`——正是 `readOnlyRootFilesystem` 语义。B1 只实证了 `chmod 555` 的宿主模拟，这里是真正的容器只读根文件系统。

### 实证 E — 同一 k8s 形状，`data_dir` 改为挂载点**子目录**

```
$ docker run --rm --read-only --user 65532:65532 --tmpfs /tmp -v <config data_dir=/var/lib/halro/data> -v hv-k2:/var/lib/halro halro-v2:test init --config /etc/halro/config.yaml
Halro initialized
```

再用同一个卷起 `serve`：

```
$ docker run -d --name halro-v2-serve --read-only --user 65532:65532 --tmpfs /tmp \
    -v <config data_dir=/var/lib/halro/data> -v hv-k2:/var/lib/halro halro-v2:test serve --config /etc/halro/config.yaml
$ docker logs halro-v2-serve | tail -3
{"level":"INFO","msg":"listener started","address":"127.0.0.1:8081"}
{"level":"INFO","msg":"listener started","address":"127.0.0.1:9090"}
{"level":"INFO","msg":"listener started","address":"127.0.0.1:8080"}
$ docker ps --filter name=halro-v2-serve --format '{{.Status}}'
Up 8 seconds (healthy)
$ docker exec halro-v2-serve /usr/local/bin/halro healthcheck --url http://127.0.0.1:8080/health/ready
healthcheck exit=0
```

**k8s manifest 的安全上下文、挂载点、PVC 形状全部没问题——它在正确的 `data_dir` 下完整跑通并 ready。**
该 manifest 不含 ConfigMap，本身不规定 `data_dir`；它只在操作者照 `operator-guide:497` 填值时才坏。B1 把它列为「同源缺陷工件」是**归因错误**：错误源只有一个。

### 实证 F — 镜像自带的默认布局其实是对的（最强的一条证伪）

不改 `configs/config.example.yaml` 的 `data_dir: "./data"`（`:66`），只把 master key 指到卷内，在 k8s 只读形状下跑：

```
$ docker run --rm --read-only --user 65532:65532 --tmpfs /tmp -v <config-relative.yaml> -v hv-rel:/var/lib/halro halro-v2:test init --config /etc/halro/config.yaml
Halro initialized
$ docker run --rm -v hv-rel:/v busybox ls -la /v
drwxr-xr-x 1 65532 65532  .
-rw------- 1 65532 65532  .halro-publication-3350bddd368ff7b3.lock
drwx------ 1 65532 65532  data
-rw------- 1 65532 65532  master.key
```

`WORKDIR /var/lib/halro`（`Dockerfile:29`）让相对路径 `./data` 解析成 `/var/lib/halro/data`，锁落在卷内，一切正常。
**结论：`Dockerfile` 的布局设计本来就是「挂载点为父、data 为子」，与 `backup-restore.md` 一致。** B1 的「镜像开箱只支持这个坏布局」被证伪；把 Dockerfile 列入受损工件清单是错的。真正打破它的是 `operator-guide.md:497` 那句 "Container configuration must use `/var/lib/halro` for `storage.data_dir`"——它把一个能工作的相对默认值改写成了一个不能工作的绝对值。

### 实跑矩阵汇总

| # | rootfs | uid | 卷挂载点 | `data_dir` | 结果 |
|---|---|---|---|---|---|
| A | rw | 65532 | `/var/lib/halro` | `/var/lib/halro` | ❌ `permission denied` |
| B | rw | 65532 | 无 | `/var/lib/halro` | ❌ `permission denied` |
| C | rw | 0 | `/var/lib/halro` | `/var/lib/halro` | ✅ 越过锁（root 绕过） |
| D | **ro** | 65532 | `/var/lib/halro` | `/var/lib/halro` | ❌ `read-only file system` |
| E | **ro** | 65532 | `/var/lib/halro` | `/var/lib/halro/data` | ✅ `Halro initialized` + serve healthy |
| F | **ro** | 65532 | `/var/lib/halro` | `./data`（仓库自带默认值） | ✅ `Halro initialized` |

---

## 4. systemd（**未实跑**，静态等价性论证）

本机是 macOS，无 systemd。以下为静态论证，明确标注未实跑：

- `deploy/systemd/halro-aws-kms.service:26` `ProtectSystem=strict` 使整个文件系统层级（除 `/dev`、`/proc`、`/sys`）只读；`:27` `ReadWritePaths=/var/lib/halro` 单独把该子树重新挂成可写。
- 因此 `/var/lib` 只读、`/var/lib/halro` 可写——**与实证 D/E 的容器形状在文件系统语义上等价**（D 的 `read-only file system` 正是这一形状下会得到的 errno EROFS）。
- 若 `data_dir=/var/lib/halro`：锁落在只读的 `/var/lib` → 失败。
- 若 `data_dir=/var/lib/halro/data`：锁落在可写的 `/var/lib/halro` → **成功**（等价于实证 E）。

**关键差异（这是我对 B1 的主要不同意见）**：unit 文件不规定 `data_dir`，而 `operator-guide.md:497` 明确把自己限定为 "**Container** configuration"——它不覆盖裸机 systemd。仓库里没有任何文档告诉 systemd 操作者用 `/var/lib/halro` 当 `data_dir`；反倒是 `README.md:81-83` 和 `backup-restore.md:169-197` 都在说「挂父目录、`data_dir` 取子目录」。所以这份 unit 在正确布局下是**对的**：`ReadWritePaths=/var/lib/halro` 恰好把发布锁需要的那一层放开了。

B1 对 systemd 的判定基于「操作者会把 `data_dir` 设成 `/var/lib/halro`」这个未被任何文档支持的推定。**作为独立工件，systemd unit 的这一条 REFUTED。**
（可改进之处存在但不属于本条发现：unit 没有 `StateDirectory=halro`，`/var/lib/halro` 的创建与属主留给了操作者，`ReadWritePaths` 指向不存在的路径会让 systemd 启动失败。这是另一条建议级问题。）

---

## 5. 对「P0 · 阻塞发布」定级的单独表态

**表态：缺陷成立，但 P0 高估一档。建议 P1 + 「发布前必修」，而不是 P0 阻塞。**

支持降级的事实：

1. **修复面是一句话，不是四份工件。** 唯一需要改的是 `operator-guide.md:497`（把 `/var/lib/halro` 改成 `/var/lib/halro/data`，或直接删掉这句让相对默认值生效）。Dockerfile 不用动（实证 F 证明它已经是对的），k8s manifest 不用动（实证 E），systemd unit 不用动（§4）。B1 提出的「四处统一改 + 改 `VOLUME`/`WORKDIR` 为 `/var/lib/halro-volume`」是**过度修复**，反而会破坏实证 F 里已经工作的默认路径。
2. **失效方式是理想的 fail-closed。** 第一次启动、写任何数据之前、进程立刻退出并打印带完整路径的报错。没有静默降级、没有数据损坏、没有半初始化状态、没有安全后果、没有账目影响。可用性缺陷里这是最轻的一档。
3. **正确答案已经在仓库里，且在两个地方。** `README.md:81-83` 与 `backup-restore.md:169-197` 都写对了。先读 README 的操作者不会踩到。
4. **不涉及代码改动，因而不影响任何已签名的二进制或镜像。** `halro-container.tar.gz` 本身没问题——是随附文档的一句话有问题。文档可以在发布后单独修，不需要重新出包。
5. **没有既有部署需要迁移**（pre-1.0.0，无 GitHub Release）。

支持维持阻塞的事实（我认为这才是这条发现真正的价值）：

1. **照官方容器小节做的人 100% 起不来**，且第一条报错不解释「这是 `data_dir` 的父目录」，紧接着 `halro doctor` 还会把人引向不存在的锁持有者（B1-04）。这对「开箱」是实打实的伤害。
2. **仓库自相矛盾**：同一份文档集里两处说 A、一处说 not-A，且没有任何门禁把它们对齐。
3. **CI 从不启动容器**（`ci.yml:141-155` 已核实），所以这类错误可以无声地回来。**这条守护缺失比缺陷本身更值得记。**

**我的建议裁决**：
- B1-01 判 **P1 · 发布前必修**，修复内容限定为 `docs/guides/operator-guide.md:497` 一行 + 该小节补一条 `init` 步骤（与 B1-02 合并）。
- 从工件清单里**撤下 Dockerfile、systemd unit、k8s manifest 三项**——它们不是缺陷源，写进发布说明会误导后续修复者去改坏已经正确的东西。
- 单独立一条 **P2 · CI `container` job 增加「挂卷 + init + 就绪探测」步骤**，这是唯一能防止复发的东西，且它的缺失才是系统性问题。

---

## 6. 附录

### 6.1 读过的文件

- `internal/store/lock/lock_unix.go`（全文）
- `internal/store/lock/lock_test.go`（目录清点）
- `internal/app/kms_master_key.go:414-418`（`AcquireInitialization` 调用点）
- `Dockerfile`（全文）
- `configs/config.example.yaml`（全文，重点 `:66`、`:82`）
- `docs/guides/operator-guide.md:440-503`（Troubleshooting + Optional container image）
- `docs/guides/backup-restore.md:150-215`（Docker and Kubernetes storage layout）
- `README.md:70-95`
- `deploy/systemd/halro-aws-kms.service`（全文）
- `deploy/kubernetes/halro-aws-kms.yaml:1-90`（全文）
- `.github/workflows/ci.yml:130-165`（`container` job）
- `.github/workflows/release.yml:185-225`（容器打包段）
- `docs/review/260811/releases.1.0.0/role-prompts.md`（§1、§8）
- `docs/review/260811/releases.1.0.0/findings/B1.md`（全文）

### 6.2 运行过的命令

```bash
# 机制核对
go build -o <scratch>/v2/halro ./cmd/halro
<scratch>/v2/halro init --config <scratch>/v2/local/cfg.yaml   # 锁落在 data_dir 父目录
ls -la <scratch>/v2/local/parent

# 镜像
docker build -t halro-v2:test .
cid=$(docker create halro-v2:test); docker export $cid | tar -tvf - | grep var/lib   # /var/lib = root:root 0755

# 实证 A（operator-guide.md:489-495 逐字）
docker run --rm --user 65532:65532 \
  -v "$PWD/config.yaml:/etc/halro/config.yaml:ro" \
  -v "$PWD/master.key:/run/secrets/halro-master.key:ro" \
  -v halro-data-v2:/var/lib/halro -p 18080:8080 -p 18081:8081 halro-v2:test

# 实证 B/C/D/E/F（矩阵，见 §3）
docker run --rm --user 65532:65532 ... halro-v2:test init --config /etc/halro/config.yaml     # 无卷
docker run --rm --user 0:0        ... -v hv-root:/var/lib/halro  ... init                      # root
docker run --rm --read-only --tmpfs /tmp --user 65532:65532 -v hv-k1:/var/lib/halro ... init   # k8s 形状 / 挂载点
docker run --rm --read-only --tmpfs /tmp --user 65532:65532 -v hv-k2:/var/lib/halro ... init   # k8s 形状 / 子目录
docker run --rm --read-only --tmpfs /tmp --user 65532:65532 -v hv-rel:/var/lib/halro ... init  # 仓库默认 ./data
docker run -d --name halro-v2-serve --read-only --tmpfs /tmp ... serve
docker logs halro-v2-serve; docker ps --filter name=halro-v2-serve --format '{{.Status}}'
docker exec halro-v2-serve /usr/local/bin/halro healthcheck --url http://127.0.0.1:8080/health/ready
docker run --rm -v hv-rel:/v busybox ls -la /v

# 收尾
docker rm -f halro-v2-serve
docker volume rm -f halro-data-v2 hv-root hv-child hv-e1 hv-e2 hv-k1 hv-k2 hv-rel
docker rmi -f halro-v2:test busybox:latest
git status --porcelain
```

清理已确认：`docker images | grep halro-v2` 无匹配；残留的 `halro-drill`、`halro-b1-data`、
`buildx_buildkit_halro-multiarch0` 等属于其他角色，未触碰。

### 6.3 未能验证的事项

- **systemd 未实跑**（本机 macOS）。§4 是基于 `ProtectSystem=strict` 语义 + 实证 D/E 的容器只读形状做的等价性论证，不是实测。建议在 Linux 上补一次真实 unit 启动，验证两点：(a) `data_dir=/var/lib/halro/data` 能启动；(b) `ReadWritePaths` 指向不存在目录时 systemd 的行为。
- **真实 Kubernetes 未实跑**。实证 D/E 用 `--read-only` + 具名卷模拟 `readOnlyRootFilesystem` + PVC，文件系统语义等价（errno 一致），但未在真实 kubelet/CSI 下验证 `fsGroup: 65532` 对 PVC 根目录的 chgrp 行为。该行为若不生效，实证 E 的结论在真实 k8s 上需重测。

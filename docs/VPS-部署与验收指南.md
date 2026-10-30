# VPS 部署与验收指南

本指南面向「在 VPS 上把这个单二进制服务跑起来，并逐项验证它确实工作」的场景。
README 已覆盖 Docker 与二进制的总览，这里补上**针对本项目最近改动（P1-0 调度器重构、P1-1 `ops_tasks` 表与状态机）的验收清单**，以及一份可直接照抄的 systemd 部署脚本。

> 涉及代码改动：
> - `internal/tasksched` 通用调度器（P1-0）——`huntsvc` 接入它，业务逻辑未动。
> - `internal/store/opstask.go` + `internal/httpapi/tasks.go`（P1-1）——新增 `ops_tasks` 表、`ops_tasks` 状态机、HTTP 接口 `GET /api/tasks` 等，以及启动时的 `ResetRunningOpsTasks` 恢复。
> 验收要能证明：表与索引已建、`/api/tasks` 未登录必须 401、重启能恢复卡在 `running` 的任务。

---

## 1. 前提

- 目标机：Linux（甲骨文免费机是 **ARM64 / Ampere A1**，普通 VPS 多为 **amd64**）。
- 不需要在目标机装 Go / Node：交叉编译出单文件即可。
- 一个能 SSH 上去的账号；建议用**专用 IAM 用户**的 OCI 密钥来做功能验证，别拿管理员密钥试。

---

## 2. 构建（本地交叉编译）

仓库里 Go 工具链在 `_tools/go`，SQLite 用纯 Go 实现（`modernc.org/sqlite`），
**无需 CGO**，直接静态编译。前端已提交进 `internal/web/dist`，`go build` 会把它嵌进二进制。

```bash
cd oci-manager

# 国内网络若拉不到依赖，先设镜像（首次编译会下载 modernc.org/libc 等）
export GOPROXY=https://goproxy.cn,direct

# 甲骨文免费机 / ARM 服务器
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  ./_tools/go/bin/go build -ldflags "-s -w -X main.version=0.3.0" \
  -o _dist/ocicore-linux-arm64 ./cmd/server

# 通用 x86_64 VPS
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  ./_tools/go/bin/go build -ldflags "-s -w -X main.version=0.3.0" \
  -o _dist/ocicore-linux-amd64 ./cmd/server

ls -lh _dist/
```

产物约 10–12 MB，单个文件、无运行时依赖。

> 如果目标机有 Go 工具链、想直接在上面编，跳过交叉编译，直接 `go build -o ocicore ./cmd/server` 即可。

---

## 3. 上传与目录布局

```bash
# 选对应架构
scp _dist/ocicore-linux-arm64 你的用户@你的服务器:/opt/ocicore/ocicore
ssh 你的用户@你的服务器

sudo mkdir -p /opt/ocicore /opt/ocicore/data
sudo chown -R 你的用户:你的用户 /opt/ocicore
chmod +x /opt/ocicore/ocicore
```

最终布局：

```
/opt/ocicore/
├── ocicore            # 单二进制
└── data/              # 运行时生成：oci-tools.db + master.key
```

> ⚠️ `data/` 里的 `master.key` 是全部 OCI 私钥的唯一解密钥匙。备份它、别进版本库、别随日志外泄。

---

## 4. 启动

### 4.1 先前台跑一次，确认能起

```bash
cd /opt/ocicore
OCICORE_ADDR=127.0.0.1:8080 \
OCICORE_DATA_DIR=/opt/ocicore/data \
./ocicore
```

正常会看到：

```
service started addr=127.0.0.1:8080 version=... data=/opt/ocicore/data
尚未初始化，请在浏览器中打开面板完成首次设置
```

> 默认只听回环（`127.0.0.1:8080`）——这是刻意的安全默认。远程访问走 SSH 隧道或 TLS 反代，不要直接把 8080 映射给公网。

### 4.2 用 systemd 常驻（推荐）

`/etc/systemd/system/ocicore.service`：

```ini
[Unit]
Description=OCI Core 自托管面板
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=你的用户
WorkingDirectory=/opt/ocicore
ExecStart=/opt/ocicore/ocicore
Environment=OCICORE_ADDR=127.0.0.1:8080
Environment=OCICORE_DATA_DIR=/opt/ocicore/data
# 可选：把主密钥直接注入，便于外部密钥管理；不配则自动生成 master.key
# Environment=OCICORE_MASTER_KEY=000102030405060708090a0b0c0d0e0f...
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ocicore
sudo systemctl status ocicore --no-pager
```

---

## 5. 访问（安全第一）

### 方案 A：SSH 隧道（最省事，不暴露端口）

本地执行：

```bash
ssh -N -L 8080:127.0.0.1:8080 你的用户@你的服务器
```

然后本地浏览器开 `http://127.0.0.1:8080`。

### 方案 B：反向代理 + TLS（需要公网访问时）

Nginx / Caddy 在前，只暴露 443。此时**必须**在 `ocicore.service` 里加：

```ini
Environment=OCICORE_ADDR=0.0.0.0:8080
Environment=OCICORE_TRUSTED_PROXIES=127.0.0.0/8,10.0.0.0/8
```

> 没有真实反代却开着 `TRUST_PROXY` 类开关，任何人都能伪造 `X-Forwarded-For` 绕过登录限流。不要把 `0.0.0.0/0` 写进 `TRUSTED_PROXIES`（启动会直接拒绝）。

---

## 6. 首次设置

浏览器打开后按引导：

1. 创建管理员账号（argon2id 口令）。
2. 绑定 TOTP（扫码加入验证器）。
3. 进入「设置」按需调整同步间隔、凭据复查间隔、审计保留期。

完成后 `GET /api/status` 的 `setupRequired` 会从 `true` 变 `false`。

---

## 7. 验收清单（逐项核对）

下面所有 `curl` 都假设走 SSH 隧道（本地 8080）。

### 7.1 健康检查（公开端点）

```bash
curl -s http://127.0.0.1:8080/api/status | head -c 400; echo
```

首次未设置应含 `"setupRequired":true`；设置完成后含 `"setupRequired":false`、版本号等。
这个端点是**刻意公开**的（登录页靠它判断要不要走初始化），不代表漏洞。

### 7.2 鉴权闸门（P1-1 关键点）

`/api/tasks` 是运维任务接口，**未登录必须 401**：

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8080/api/tasks
# 期望：401
```

若返回 200 或 404，说明路由或鉴权接线有问题，必须回查 `internal/httpapi/server.go` 的 `GET /api/tasks` 是否包了 `requireAuth`。

### 7.3 数据库完整性（P1-1 关键点）

SSH 到服务器，看库里确有 `ops_tasks` 表与两个索引：

```bash
sqlite3 /opt/ocicore/data/oci-tools.db ".schema ops_tasks"
sqlite3 /opt/ocicore/data/oci-tools.db "SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='ops_tasks';"
```

期望看到：

- 表 `ops_tasks`，列含 `id, account_id, kind, name, state, spec, progress, attempts, next_at, last_error, created_at, updated_at`。
- 索引 `idx_ops_due (state, next_at)` 与 `idx_ops_account (account_id)`。

> 没有 sqlite3 客户端时，用 `strings oci-tools.db | grep -i idx_ops` 粗略确认索引名存在也行；但 `.schema` 最准。

### 7.4 启动恢复（P1-1 关键点）

验证「上轮被杀、停在 running 的任务，重启后会被重置回 pending」：

```sql
-- 手动把一行 ops 任务标成 running（没有就先随便造一行）
UPDATE ops_tasks SET state='running' WHERE id='<某id>';
```

然后重启服务：

```bash
sudo systemctl restart ocicore
sudo journalctl -u ocicore --since "1 min ago" | grep -i "重新排队"
```

期望日志出现「已将上次中断的运维任务重新排队 count=N」，且该行 `state` 回到 `pending`：

```bash
sqlite3 /opt/ocicore/data/oci-tools.db "SELECT id, state FROM ops_tasks WHERE id='<某id>';"
# 期望 state=pending
```

### 7.5 状态机白名单（P1-1）

用测试账号走一遍非法转换应被拒。最省事的方式是调接口让系统自己跑：
先 `POST /api/tasks/<id>/action` 对一个 `succeeded` 的任务发 `pause`，应当返回 **409** 且错误码 `illegal_transition`（终态不能再动）。这部分逻辑在单测 `TestOpsStateMachineMatrix` 已穷举 36 种组合覆盖，部署侧只需确认接口能返回 409 而非 500。

### 7.6 功能冒烟（整体）

用专用测试账号跑一次**最小风险**的功能，验证端到端链路（签名、代理、SQLite 读写）通畅，然后清理：

1. 添加账号 → 粘贴 OCI config → 测试连接（走你绑的代理）。
2. 进「容量监控」查一次某规格容量（只读接口，安全）。
3. 不要急着开「容量守候」真实创建——那是会持续打 OCI 创建接口的高频行为。
   想验证守候链路，可观察其任务行进入 `pending`/`running` 状态机而不报错即可，验证完立即 `cancel`。
4. 验证完在界面删除该测试账号（删除会级联清掉它的 `ops_tasks` 行）。

### 7.7 后台调度在跑（P1-0）

确认容量守候调度器、账号复查、审计清理、风险预算清理等后台 goroutine 已起：

```bash
sudo journalctl -u ocicore --since "5 min ago" | grep -iE "服务已启动|重新排队|风险预算"
```

只要 `服务已启动` 出现、且 5 分钟内没有 `panic`/`启动失败`，调度循环就是健康的。

---

## 8. 安全与备份

- **备份 `master.key`**：`tar czf ocicore-backup.tar.gz -C /opt/ocicore data`，把文件当密钥保管。
- **绝不 `docker compose down -v` / 误删 `data/`**：`master.key` 丢失 = 所有已存 OCI 私钥永久解不开。
- **默认只听回环**；公网访问一律走 TLS 反代 + `TRUSTED_PROXIES` 显式网段。
- 部署后**立刻**完成首次设置，避免「已上线但无管理员」被扫描器抢注。

---

## 9. 升级 / 回滚

升级（保留数据目录）：

```bash
scp _dist/ocicore-linux-arm64 你的用户@你的服务器:/opt/ocicore/ocicore.new
ssh 你的用户@你的服务器
sudo systemctl stop ocicore
mv /opt/ocicore/ocicore.new /opt/ocicore/ocicore && chmod +x /opt/ocicore/ocicore
sudo systemctl start ocicore
```

数据库迁移在启动时自动执行；回滚只需把上一个二进制换回去，数据目录不变。
升级前先按 §8 备份 `data/`。

---

## 10. 磁盘紧张的机器上跑 dev 分支（Docker，实测）

前面几节假设目标机有构建余量。如果机器很小（实测一台 Armbian：`/` 只剩
943MB、Docker 可回收仅 89MB、内存可用 896MB），仓库根目录的多阶段
`Dockerfile` 走不通——它要拉 `node:24-alpine` + `golang:1.26-alpine`（500MB 以上），
再加 npm 依赖与 go 模块缓存，峰值 1GB 以上；写满根分区会连带影响同机的其他容器。

改用「构建机交叉编译 + 极简运行时镜像」，镜像约 25MB，目标机构建几秒完成。
完整步骤与验收命令见 **`deploy/vps-dev/README.md`**，这里只记实测结论。

### 10.1 与生产实例隔离

dev 版本要和生产版本并排跑，四条都必须错开：

| 项 | 生产（0.4.14） | dev |
|---|---|---|
| 目录 | `/opt/ocicore` | `/opt/ocicore-dev` |
| 容器名 | `ocicore` | `ocicore-dev` |
| 镜像 | `ghcr.io/jsongmax/oci-core:0.4.14` | `ocicore:dev` |
| 端口 | `127.0.0.1:8080` | `127.0.0.1:8081` |
| 数据卷 | `ocicore_ocicore-data` | `ocicore-dev-data`（副本） |

**数据卷必须是副本，不能共用**。两个进程同时写同一个 SQLite 会在 WAL 上互锁，
更麻烦的是 dev 版启动时跑的迁移会改生产库的表结构。复制时要带 `master.key`
（否则已存的 OCI 私钥解不开），并且 `chown -R 10001:10001`——容器以 uid 10001
运行，`cp` 出来的目标目录属主是 root，没有写权限时 SQLite 连 WAL 都建不出来。

### 10.2 实测结果

| 项 | 结果 |
|---|---|
| 容器 | `ocicore-dev` → `healthy`，与生产的 `ocicore` 并存 |
| 版本 | `/api/status` 返回 `"version":"dev-<commit>"`（二进制里编进提交号，用来确认跑的是哪个提交） |
| 鉴权 | `/api/tasks`、`/api/jobs`、`/api/instances`、`/api/accounts`、`/api/logs/stream`、`/api/geo/status`、`/api/cf/zones` 全 401；`POST /api/ops/change-ip` 无 CSRF 403、带 CSRF 未登录 401 |
| 迁移 | dev 库 **20 表 / 20 索引**（部署时生产还是 0.4.14，11 表 / 10 索引）。生产后来升到 0.5.4（12 表），dev 仍多出 8 张本地表：`ops_tasks`、`cf_zones`、`cf_records`、`ip_cache`、`risk_budget`、`account_health`、`account_meta`、`account_tags` |
| `ops_tasks` | 22 列，索引 `idx_ops_due` / `idx_ops_account` 齐全 |
| 日志 | 0 条 ERROR |
| 磁盘 | 全程增加 34MB（895MB 可用 → 861MB）；dev 容器重建一次再多用约 25MB |

更新 dev 实例（换新二进制）不需要重新准备数据卷，迁移会在启动时自己补：

```bash
# 构建机上
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 ./_tools/go/bin/go build -trimpath \
  -ldflags "-s -w -X main.version=dev-$(git rev-parse --short HEAD)" \
  -o _dist/deploy-dev/ocicore ./cmd/server
scp _dist/deploy-dev/ocicore root@<vps>:/opt/ocicore-dev/ocicore.new

# VPS 上（留一份 .prev 便于即时回滚）
cd /opt/ocicore-dev && cp -a ocicore ocicore.prev \
  && mv ocicore.new ocicore && chmod 755 ocicore \
  && docker compose up -d --build
```

> 生产实例若跟随上游 tag 升级，先确认 GHCR 上**该版本镜像已发布**
> （`docker manifest inspect ghcr.io/jsongmax/oci-core:<ver>`）。上游打 tag 与发镜像
> 并不同步——实测 v0.5.5 的 tag 已存在但镜像未发布，这种情况生产保持上一个已发布
> 版本即可，不要为了一个前端小改动去自建镜像，否则会丢掉「跟官方镜像」的维护模型。

### 10.3 几个容易踩的点

- **`docker compose` 会报 `volume "ocicore-dev-data" already exists but was not created by Docker Compose`**。数据卷是手动建的，标 `external: true` 即可；顺带的好处是 `docker compose down -v` 删不掉它——里面的 `master.key` 丢不起。
- **`json-file` 日志驱动默认无限增长**。不到 900MB 的磁盘上，一个跑几十天的面板足以写满根分区。compose 里显式给 `max-size: "10m"` / `max-file: "2"`。
- **本机 `HTTP_PROXY` 会让 `curl 127.0.0.1:8081` 也走代理**，代理连不上就返回 `502 upstream connect failed`，看起来像容器挂了。本机所有探测加 `--noproxy '*'`，浏览器把 `127.0.0.1` 加进代理例外。
- **裸 `ssh -L` 隧道会静默断开**（实测跑满 6 小时后被对端 `Connection reset by peer`）。断了之后本机端口消失、浏览器「拒绝连接」，而容器其实好好的。用 `while true; do ssh -N -L ...; sleep 5; done` 配 `ServerAliveInterval=30` 自动重连。

### 10.4 验收前要准备的东西

数据是从生产卷复制的，但**生产库本身 `accounts` 是 0 行**——还没有任何 OCI 账号。
换 IP、放行端口、500Mbps 这些要真调 Oracle 接口的功能，得先在面板里添加一个
真实账号（租户 OCID / 用户 OCID / 私钥 / 指纹 / 区域）才能验。建议用专用 IAM
用户，别拿管理员密钥试。

### 10.5 用官方镜像升级生产实例（实测 0.4.14 → 0.5.4）

生产实例跑的是 GHCR 上的官方镜像时，升级不需要构建、不需要源码，只要 25MB 磁盘：

```bash
# 0. 先备份。WAL 模式下直接 cp 库文件不安全，用 SQLite 在线备份 API
python3 - <<'PY'
import sqlite3
src = sqlite3.connect('file:/var/lib/docker/volumes/ocicore_ocicore-data/_data/oci-tools.db?mode=ro', uri=True)
src.backup(sqlite3.connect('/tmp/oci-backup/oci-tools.db'))
PY
cp /var/lib/docker/volumes/ocicore_ocicore-data/_data/master.key /tmp/oci-backup/
tar czf /tmp/ocicore-backup.tar.gz -C /tmp/oci-backup .
# 立刻下载走：/tmp 重启就没了，而里面有 master.key

# 1. 拉新版本
docker pull ghcr.io/jsongmax/oci-core:0.5.4

# 2. 改 compose 的 image 与 build.args.VERSION，然后重建
cd /opt/ocicore
sed -i 's|oci-core:0.4.14|oci-core:0.5.4|; s|VERSION: "0.4.14"|VERSION: "0.5.4"|' docker-compose.yml
docker compose up -d
```

**验收**（本次实测值）：容器 `healthy`、`/api/status` 返回 `"version":"0.5.4"`、
迁移后 11 表/10 索引 → **12 表/12 索引**（新增 `jobs` + 两个索引）、`users` 数据保留、
错误口令返回 401 而非 500、日志 0 ERROR。

**回滚**：compose 里 tag 改回去 + `docker compose up -d`，旧镜像仍在本地不用重拉。
注意迁移加的表不会自动删除——`jobs` 对 0.4.14 无害（它不认识也不读），
要完全还原得从备份恢复库。

**一个容易留坑的地方**：`/opt/ocicore` 通常是上游的 git clone。用 `sed` 改
compose 会让工作区多出一处未提交修改，下次 `git pull` / `git checkout` 会撞上。
要么把这次改动提交到本地分支，要么升级前先 `git stash`。

# 磁盘紧张的小机器上部署 dev 分支（离线二进制版）

这套文件是为「目标机没有构建余量、但又必须跑上 dev 分支」的场景准备的。
和仓库根目录的 `Dockerfile`（多阶段构建）是两条路，**不要混用**。

## 什么时候用它

仓库根的 `Dockerfile` 会在目标机上跑 `npm ci` + `vite build` + `go build`，需要：

- 拉 `node:24-alpine` 与 `golang:1.26-alpine`（合计 500MB 以上）
- npm 依赖、go 模块缓存、构建中间层（峰值 1GB 以上）

如果目标机满足「磁盘可用 < 1.5GB」或「内存 < 1GB」，多阶段构建要么失败，
要么把根分区写满——后者会连带影响同一台机器上的其他容器（尤其它们的 SQLite）。
实测的一台 Armbian 机器就是这种情况：`/` 只剩 943MB、Docker 可回收仅 89MB。

## 它怎么工作

后端二进制在**构建机**上交叉编译好（`CGO_ENABLED=0`，纯静态，SQLite 是
`modernc.org/sqlite` 不需要 libc），运行时镜像只把它拷进 `alpine`。
镜像约 25MB，目标机上的构建几秒完成，不需要 node / go / 源码。

前端不需要单独处理：`internal/web/dist` 是入库的，`go:embed` 已经把它编进二进制。

## 步骤

### 1. 构建机上交叉编译

```bash
export GOPROXY=https://goproxy.cn,direct

# 按目标机架构选一个：uname -m 看到 aarch64 用 arm64，x86_64 用 amd64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  ./_tools/go/bin/go build -trimpath \
  -ldflags "-s -w -X main.version=dev-$(git rev-parse --short HEAD)" \
  -o _dist/deploy-dev/ocicore ./cmd/server
```

> 版本号写进 `main.version`，面板 `/api/status` 与「关于」里能看到，
> 验收时用来确认跑的是哪个提交。

### 2. 上传

```bash
scp deploy/vps-dev/Dockerfile deploy/vps-dev/docker-compose.yml \
    _dist/deploy-dev/ocicore  root@<vps>:/opt/ocicore-dev/
```

### 3. 准备数据卷

**不要和生产实例共用数据卷。** 两个进程同时写同一个 SQLite 会在 WAL 上互锁，
更麻烦的是 dev 版启动时跑的迁移会改生产库的表结构。

从生产卷复制一份（含 `master.key`，否则已保存的 OCI 私钥解不开）：

```bash
docker volume create ocicore-dev-data
docker run --rm \
  -v ocicore_ocicore-data:/from:ro \
  -v ocicore-dev-data:/to \
  alpine:3.21 sh -c 'cp -a /from/. /to/ && chown -R 10001:10001 /to'
```

`chown` 不能省：容器以 uid 10001 运行，而 `cp` 出来的目标目录属主是 root，
没有写权限时 SQLite 连 WAL 都建不出来。

想从空库开始就跳过这一步，让容器自己初始化。

### 4. 起服务

```bash
cd /opt/ocicore-dev
docker compose up -d --build
docker compose ps
docker compose logs -f
```

### 5. 访问

compose 只绑 `127.0.0.1:8081`。这个面板持有全部 Oracle 租户的控制权，
不要挂公网。本机开隧道：

```bash
ssh -L 8081:127.0.0.1:8081 root@<vps>
# 然后浏览器打开 http://127.0.0.1:8081
```

## 验收要点

```bash
# 存活 + 版本
curl -s http://127.0.0.1:8081/api/status
# {"setupRequired":false,"authenticated":false,...,"version":"dev-524c435"}

# 路由已注册且受保护（404 说明没注册，401/403 才是对的）
for p in tasks jobs instances accounts; do
  echo "$p -> $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/api/$p)"
done
# POST /api/ops/change-ip 无 CSRF 头应 403，带 CSRF 未登录应 401

# 迁移落库：dev 库应比 0.4.14 多出 ops_tasks / jobs / cf_* 等 9 张表
python3 -c "
import sqlite3
con = sqlite3.connect('file:/var/lib/docker/volumes/ocicore-dev-data/_data/oci-tools.db?mode=ro', uri=True)
print(sorted(r[0] for r in con.execute(\"select name from sqlite_master where type='table'\")))
"
```

## 与生产实例的隔离

| 项 | 生产（0.4.14） | dev |
|---|---|---|
| 目录 | `/opt/ocicore` | `/opt/ocicore-dev` |
| 容器名 | `ocicore` | `ocicore-dev` |
| 镜像 | `ghcr.io/jsongmax/oci-core:0.4.14` | `ocicore:dev` |
| 端口 | `127.0.0.1:8080` | `127.0.0.1:8081` |
| 数据卷 | `ocicore_ocicore-data` | `ocicore-dev-data`（副本） |

## 更新与回滚

```bash
# 更新：重新交叉编译、scp 覆盖二进制、重建容器
docker compose up -d --build

# 回滚：把上一个二进制放回去再 up
# 数据卷不受影响（external，compose down -v 也删不掉）
```

## 清理

```bash
cd /opt/ocicore-dev && docker compose down
docker volume rm ocicore-dev-data   # 只有确定不要里面的数据时才删
docker rmi ocicore:dev
```

## 一个已知限制

`alpine` 基础镜像里的 `ocicore` 是静态二进制，但 `HEALTHCHECK` 依赖 `wget`。
如果你把基础镜像换成 `scratch`，健康检查要改成别的方式，否则容器会一直是
`unhealthy`——它不会影响服务本身，但会让 `docker ps` 的结论失真。

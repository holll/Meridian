<div align="center">

# Meridian

轻量级 Emby 反向代理管理面板
单端口服务 + 嵌入式 SPA 前端，开箱即用

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![SQLite](https://img.shields.io/badge/SQLite-embedded-003B57?logo=sqlite&logoColor=white)](https://pkg.go.dev/modernc.org/sqlite)
[![CI](https://github.com/holll/Meridian/actions/workflows/ci.yml/badge.svg)](https://github.com/holll/Meridian/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

</div>

## 界面预览

| 仪表盘 | 站点管理 | 故障诊断 |
|:---:|:---:|:---:|
| ![仪表盘](docs/dashboard.png) | ![站点管理](docs/sites.png) | ![故障诊断](docs/diagnostics.png) |

## 这是什么

Meridian 是一个专为 Emby 媒体服务器设计的反向代理管理面板（Emby reverse proxy management panel）。它解决的核心问题是：**当你需要在一台机器上管理多个 Emby 反代站点时，不想手写 Nginx 配置，不想逐个维护 UA 伪装规则，也不想自己实现流量计量和限速。**

Meridian 把这些事情打包成一个单二进制程序，带管理界面，带实时监控，开箱可用。所有站点共享同一个面板端口，通过 URL 路径前缀区分（如 `/s/site1/`、`/s/site2/`）。

## 友链

- [NodeSeek](https://www.nodeseek.com/)
- [Linux.do](https://linux.do/)

## 核心特性

| 功能 | 说明 |
|------|------|
| **多站点反代** | 每个站点通过独立的 URL 路径前缀访问，共享面板端口 |
| **路径前缀路由** | 站点由 `path_prefix`（如 `/s/emby1`）区分，无需为每个站点开放独立端口 |
| **双上游分流** | 网页/API 和播放/转码流量可分别指向不同上游 |
| **UA 伪装** | 3 种预设（Infuse / Web / 客户端）或每站自定义身份；HTTP、WebSocket 与受限播放重定向统一改写 |
| **流量管控** | 按站点统计流量、设置限速、设置配额 |
| **访问日志上报** | Relay 节点逐请求记录访问日志（状态码/延迟/字节数），批量上报 Master 入库 |
| **日志分析** | 基于访问日志的请求量趋势、状态码分布、TOP 资源/IP 排行与延迟统计 |
| **WebSocket 代理** | 完整支持 Emby 的 WebSocket 通信 |
| **SSE 实时推送** | 仪表盘数据通过 Server-Sent Events 实时更新 |
| **故障诊断** | 回源健康检测、上游 TLS 证书检查、请求头预览 |
| **JWT 认证** | Bearer Token 认证，密码 bcrypt 存储 |
| **单二进制部署** | 前端嵌入二进制，SQLite 持久化，无外部依赖 |

---

## 快速部署

### Linux / macOS — 一键安装（适用于已发布版本）

一行命令进入精简菜单：安装、更新到最新版、修改管理员密码、卸载。

**Master（管理面板）：**

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install.sh)
```

**Relay（流量节点）：**

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install-relay.sh)
```

> 首次安装从 GitHub Releases 下载最新二进制，并使用同一 Release 中的 `SHA256SUMS` 强制校验。systemd 部署默认使用独立的非 root 用户。重复运行 `install` 不会更新程序，只用于补充或重新配置面板域名。

也可以直接指定操作：

```bash
# Master：首次安装
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install.sh) install

# Master：非交互配置面板域名
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install.sh) install \
  --domain panel.example.com --email admin@example.com -y

# Master：更新（自动备份 + 健康检查 + 失败回滚）
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install.sh) update

# Master：修改管理员密码（同时轮换 JWT_SECRET）
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install.sh) password

# Master：卸载（默认保留数据，--purge 才删除）
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install.sh) uninstall

# Relay：首次安装（交互输入 MASTER_URL、RELAY_TOKEN、RELAY_NAME）
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install-relay.sh) install

# Relay：更新
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install-relay.sh) update

# Relay：卸载
bash <(curl -fsSL https://raw.githubusercontent.com/holll/Meridian/master/install-relay.sh) uninstall
```

更新和改密会在内部自动创建一致性备份、执行健康检查并在失败时回滚；这些内部操作不再作为公开菜单命令。备份默认保存在 `/opt/meridian-backups`，权限为 `0600`，请按敏感文件保管。卸载默认保留数据和备份；`--purge` 才删除数据。

反向代理请参考 `docs/nginx-site.conf` 自行配置。

### Windows

```powershell
Invoke-WebRequest -Uri "https://github.com/holll/Meridian/releases/latest/download/meridian-windows-amd64.exe" -OutFile "meridian.exe"
$env:JWT_SECRET = -join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Max 256) })
.\meridian.exe
```

> Windows 二进制下载同样依赖 GitHub Releases。没有已发布版本时，请使用源码构建。

### 从源码构建

```bash
git clone https://github.com/holll/Meridian.git && cd Meridian
go build -o meridian .
JWT_SECRET=$(openssl rand -hex 32) ./meridian
```

未配置域名时访问 `http://你的IP:9090`；配置后访问对应的 `https://面板域名`。首次打开会要求输入管理员账号、8–72 字节的密码，以及安装完成时显示的初始化令牌。也可以预先设置 `SETUP_TOKEN` 环境变量。

---

## 配置

### 命令行参数

```bash
./meridian                          # 默认 :9090，数据库在当前目录
./meridian --port 8080              # 自定义端口
./meridian --db /data/meridian.db   # 自定义数据库路径
read -r -s -p '新密码: ' ADMIN_PASSWORD; echo
printf '%s\n' "$ADMIN_PASSWORD" | ./meridian admin reset-password --db /data/meridian.db --password-stdin
unset ADMIN_PASSWORD
```

最后一个命令是供自动化使用的离线改密接口：密码只能从标准输入传入，数据库必须恰好有一个管理员。生产环境优先使用一键脚本的 `password` 操作，因为脚本还会停止服务、备份数据库、原子轮换 `JWT_SECRET`、重启并健康检查。

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `9090` | 管理面板监听端口（也是站点反代流量入口） |
| `DB_PATH` | `meridian.db` | SQLite 数据库路径 |
| `PANEL_BIND_ADDR` | `0.0.0.0` | 仅控制管理面板的绑定地址；域名模式由安装器设为 `127.0.0.1` |
| `PANEL_DOMAIN` | 空 | 安装器记录的单个管理面板域名；不作为播放回源配置 |
| `JWT_SECRET` | 进程启动时随机生成 | 至少 32 字节的 JWT 签名密钥。**生产环境必须显式设置**，否则每次重启后会话全部失效 |
| `SETUP_TOKEN` | 首次启动时随机生成 | 首次创建管理员所需的一次性初始化令牌；未设置时会写入启动日志 |
| `TRUSTED_PROXY_CIDRS` | 空 | 允许提供 `X-Real-IP`/`X-Forwarded-For` 的反向代理 CIDR，多个值用逗号分隔；不要填写不受信任的客户端网段 |
| `RELAY_TOKEN` | 空 | Relay 节点与 Master 通信的共享密钥（至少 32 字节），不设则 Relay API 禁用 |
| `ACCESS_LOG` | 空 | 访问日志路径；未设置时不记录访问日志 |
| `GEOLITE_DB_DIR` | 二进制同目录 | IP 归属数据库目录。启动时加载 `GeoLite2-Country.mmdb` + `GeoLite2-ASN.mmdb`（自动从 `github.com/P3TERX/GeoLite.mmdb` 镜像下载）+ `ip2region.xdb`（自动从 ip2region 官方源下载，提供国内 IP 城市/省份数据）；日志/分析页展示运营商（中文）、城市、省份与归属 |
| `GEOLITE_DISABLE` | `0` | 设为 `1` 关闭 Geo 识别（不下载不加载） |

### Relay 节点环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `9091` | Relay 节点监听端口 |
| `PANEL_BIND_ADDR` | `0.0.0.0` | 监听地址 |
| `MASTER_URL` | 必填 | Master 面板地址（如 `https://panel.example.com`，不带尾部斜杠） |
| `RELAY_TOKEN` | 必填 | 与 Master 的 `RELAY_TOKEN` 完全一致 |
| `RELAY_NAME` | 必填 | 全局唯一节点标识（用作 Master relay_nodes 表主键） |
| `RELAY_ISP` | 空 | 运营商标识（可选）。Master 启用 GeoLite 时根据节点公网 IP 自动识别（`telecom`/`unicom`/`mobile`/`hk`/`oversea`），可留空 |
| `ACCESS_LOG_REPORT` | `1` | 是否将逐请求访问日志上报给 Master（`0` 关闭，压测场景可关闭以降低开销） |
| `TRUSTED_PROXY_CIDRS` | 空 | 可信反向代理网段（逗号分隔）。Relay 部署在 Nginx/CDN 后时配置，访问日志取 `X-Real-IP`/`X-Forwarded-For` 真实客户端 IP；不配置则记录对端地址 |

---

## 架构说明

Meridian v2 引入主从架构，支持分布式流量节点部署：

- **Master（管理面板）**：提供 Web 管理界面、站点配置、流量统计汇总与访问日志分析，对外提供 Relay API（`cmd/meridian`，Gin + SQLite）
- **Relay（流量节点）**：从 Master 拉取站点配置，在本地运行反代引擎，定期向 Master 上报流量与访问日志（`cmd/meridian-relay`，纯 `net/http`，**无数据库**）

**典型场景**：Master 部署在内网，Relay 节点部署在不同运营商/地区，实现多线路接入和流量分担。

**v1 → v2 升级**：v1 用户可继续使用 Master 模式（无需部署 Relay），所有 v1 功能完全保留。

### Master ↔ Relay 通信

两者通过 HTTP + JSON 通信，认证用共享 `RELAY_TOKEN`（`Authorization: Bearer <token>`，常量时间比对，见 `internal/handler_relay.go` 的 `relayTokenMiddleware`）：

| Relay 接口（`/api/relay/`） | 方向 | 频率 | 说明 |
|------|------|------|------|
| `GET /sites` | Relay → Master | 30s | 拉取站点列表与全局 `ROUTE_PREFIX` |
| `POST /traffic` | Relay → Master | 60s | 上报流量增量，**兼作心跳**（更新 `last_seen`） |
| `POST /nodes/register` | Relay → Master | 启动时 | 注册节点（name/isp/version） |
| `POST /access_logs` | Relay → Master | 30s | 批量上报逐请求访问日志（每批 ≤500 条） |

Relay 侧同步器为 `internal/relay/sync.go` 的 `Syncer`：三个 ticker（sync/traffic/access log），`ctx` 取消时最终 flush；失败仅打日志不重试（下个周期自动补上）。Relay 无面板、无鉴权，仅暴露代理路由；Master 侧的请求全部走 JWT 或 RelayToken。

### 访问日志管道

```
Relay 请求处理 (internal/proxy.go StartSite handler)
  │  每请求记录：时间/站点/IP/method/path/状态码/延迟/入出字节
  ▼
AccessLogBuffer (internal/accesslog_buffer.go，有界 1000 条，满丢最旧)
  │  Syncer 每 30s Drain + 批量 POST /api/relay/access_logs（ACCESS_LOG_REPORT=0 可关）
  ▼
Master: POST /api/relay/access_logs → AddAccessLogs (internal/db.go)
  │  单事务批量 INSERT + 顺带清理 7 天前日志（access_logs 表）
  ▼
面板：GET /api/access_logs（分页明细）· GET /api/access_logs/stats（聚合分析）
  │  访问日志页（明细/筛选/分页）· 日志分析页（趋势/状态码/TOP 排行/延迟）
```

### 数据库（仅 Master，SQLite）

`internal/db.go` 的 `migrateOnce()` 自动建表/迁移，主要表：

| 表 | 用途 |
|------|------|
| `users` | 管理员账号（bcrypt 密码） |
| `sites` | 站点配置（path_prefix/target_url/playback_*/ua_mode/配额/限速） |
| `traffic_logs` | 流量统计（按站点+小时 upsert） |
| `settings` | 键值设置（迁移标记等） |
| `relay_nodes` | Relay 节点（name 唯一、last_seen、累计流量） |
| `access_logs` | 逐请求访问日志（7 天保留，ts/site_id/relay_name 索引；site_id 无外键，站点删除后日志保留） |

---

## 架构示意图

```
┌─────────────────────────────────────────────┐
│                 Meridian                      │
│                                              │
│  ┌─────────────────────────────────────────┐ │
│  │          主端口（默认 9090）              │ │
│  │                                         │ │
│  │  /api/*         → REST API（管理面板）     │ │
│  │  /css/* /js/*   → 静态资源                │ │
│  │  /api/relay/*   → Relay 注册 / 流量上报   │ │
│  │                 / 访问日志上报            │ │
│  │  /s/xxx/*       → 本地站点代理            │ │
│  └─────────────────────────────────────────┘ │
│                     ↑ RELAY_TOKEN             │
│  ┌─────────────────────────────────────────┐ │
│  │          Relay 节点（:9091）              │ │
│  │  拉取站点列表 → 本地代理                 │ │
│  │  → 上报流量/访问日志                     │ │
│  └─────────────────────────────────────────┘ │
│                                              │
│  ┌──────────────────────────────────────┐     │
│  │            SQLite (仅 Master)         │     │
│  └──────────────────────────────────────┘     │
└─────────────────────────────────────────────┘
```

| 组件 | 技术选型 |
|------|---------|
| 后端 | Go + Gin 路由 + `net/http`，模块化 `internal/` 包 |
| 前端 | 原生 HTML/CSS/JS SPA，hash 路由，`embed.FS` 嵌入 |
| 数据库 | `modernc.org/sqlite`（纯 Go，无 CGO） |
| 认证 | 自实现 HMAC-SHA256 JWT |

### 项目结构

```
Meridian/
├── cmd/
│   ├── meridian/        # Master 入口（Gin 面板 + Relay API）
│   └── meridian-relay/  # Relay 入口（纯反代，无数据库）
├── internal/            # 核心模块包
│   ├── accesslog.go     # Master 单机访问日志中间件（写文件）
│   ├── accesslog_buffer.go # Relay 访问日志有界缓冲（上报队列）
│   ├── auth.go          # JWT 认证、令牌管理
│   ├── cli.go           # 命令行工具（admin、版本）
│   ├── db.go            # SQLite 数据层、迁移、访问日志存储与聚合
│   ├── diag.go          # 故障诊断（TLS、健康探针）
│   ├── handler_accesslogs.go # 访问日志接收（relay）/ 查询 / 分析 handler
│   ├── handler_auth.go  # 认证相关 API handler
│   ├── handler_misc.go  # 仪表盘、流量、SSE handler
│   ├── handler_relay.go # Relay 注册 / 流量上报 handler、Token 中间件
│   ├── handler_site.go  # 站点 CRUD handler
│   ├── proxy.go         # 反代引擎、WebSocket、流量计量、访问日志收集
│   ├── relay/sync.go    # Relay 同步器（30s 配置 / 60s 流量 / 30s 日志）
│   ├── router.go        # Gin 路由注册、中间件
│   ├── server.go        # App 状态、登录限流、静态文件服务
│   ├── ua.go            # User-Agent 配置、Emby 授权头改写
│   ├── permissions_*.go # 文件权限（平台相关）
│   └── main_test.go     # 集成测试
├── web/
│   ├── embed.go         # Go embed 入口
│   └── static/
│       ├── index.html   # SPA 入口（导航含访问日志 / 日志分析）
│       ├── css/         # 样式
│       └── js/          # 前端逻辑（按页面拆分）
│           └── pages/   # dashboard/sites/traffic/access_logs/access_analysis/relay/diag
├── docs/                # 部署参考配置与示例 env
│   ├── master.env.example / relay.env.example
│   ├── nginx-site.conf  # Nginx 反代示例（Master + Relay）
│   ├── meridian.service # Master systemd 服务示例
│   └── meridian-relay.service  # Relay systemd 服务示例
├── install.sh           # Master 一键安装脚本
├── install-relay.sh     # Relay 一键安装脚本
├── go.mod / go.sum
└── .github/workflows/
    ├── ci.yml           # Push / PR 校验：测试 + 编译
    └── release.yml      # Tag 发布：多平台构建 + Release
```

---

## 双上游配置

每个站点可以配置两个上游地址：

| 字段 | 用途 | 示例 |
|------|------|------|
| **回源地址**（`target_url`） | 网页、API、元数据 | `https://emby.example.com` |
| **播放地址**（`playback_target_url`） | 播放、转码、直链下载 | `https://cdn.example.com` |

播放地址为可选项。不设置时所有请求走同一上游。

地址没有写协议时，Meridian 会把 `域名:443`（也兼容中文全角冒号 `：443`）识别为 HTTPS；其他端口仍默认按 HTTP 处理。HTTPS 使用非 443 端口时请明确写成 `https://域名:端口`。重定向模式会把 `https://域名:443` 和省略默认端口的 `https://域名` 视为同一播放回源。

如果上游实际部署在子路径下，可以直接填写完整基础路径，例如 `https://emby.example.com/emby`；Meridian 会把客户端请求路径安全地拼接到该基础路径。重定向播放模式只会跟随 GET/HEAD 播放请求，并要求重定向目标的协议、域名和端口与已配置播放回源一致，不会把 HTTPS 自动降级到 HTTP。

设置后以下路径会路由到播放上游：
`/Videos/`、`/emby/Videos/`、`/Audio/`、`/emby/Audio/`、`/LiveTV/`、`/emby/LiveTV/`、`/Items/.../Download`

**典型场景**：Emby 主服务器负责 API 和元数据，CDN 或专用媒体服务器负责大文件分发。

### 路径前缀

每个站点通过 `path_prefix` 隔离，默认为 `/s/` + 站点标识。客户端通过 `http://面板地址:9090/s/mysite/` 访问对应的 Emby 服务。在创建或编辑站点时，面板会校验路径前缀，确保不与 `/api`、`/css`、`/js` 等保留前缀冲突。

### UA 身份模式

每个站点可选 Infuse、Web、客户端三个预设，或选择"自定义"并填写 `User-Agent`、Emby `Client`、`Version`。自定义值会在普通 HTTP、WebSocket 以及受配置白名单约束的播放重定向请求中保持一致；`Device` 与 `DeviceId` 会原样保留。为避免请求头注入和 Emby 授权头格式损坏，自定义值只接受受限长度的可打印 ASCII 字符，`Client` 和 `Version` 不接受引号或反斜杠。

---

## 诊断功能说明

| 检测项 | 检测对象 | 含义 | 不代表什么 |
|--------|---------|------|-----------|
| **主回源健康** | 上游 `target_url` | 网络层可达性与探针结果（多探针路径，401/403/404 仍算在线；元数据接口不可用时会回退到目标根路径探针） | 不是端到端的完整业务可用性证明 |
| **播放回源健康** | `playback_target_url` 的实际生效上游 | 基于播放类路径的轻量探针结果（默认使用轻量请求，不做完整媒体拉流） | 不代表媒体链路一定可正常播放 |
| **主回源 TLS** | 主回源 HTTPS 站点证书 | 证书有效期、颁发机构展示 | 不是 Meridian 自己监听端口的证书 |
| **播放回源 TLS** | 播放回源 HTTPS 站点证书 | 仅在播放回源为独立 HTTPS 上游时单独展示 | 不负责自动签发或续期 |
| **请求头配置** | 本地 UA 配置 | 代理将发送给上游的 UA / Client / Version 值 | 不是远端回显验证 |
| **代理状态** | 本地反代进程 | 是否运行、路径前缀 | — |

当 `playback_target_url` 为空时，诊断页会明确标记"播放回源回退到主回源"；当它与 `target_url` 相同时，诊断页会复用主回源结果而不重复展示完全相同的诊断块。
播放回源健康会额外展示当前轻量探针的方法、目标 URL 和返回状态，帮助区分"播放路径可达"与"完整播放成功"这两个不同概念。

---

## 运维要点

- **JWT 密钥**：未设置 `JWT_SECRET` 时每次启动生成随机密钥，重启后会话全部失效
- **首次初始化**：数据库中没有管理员时必须提供启动日志中的初始化令牌，创建操作在数据库中原子执行
- **登录保护**：同一来源在 15 分钟内连续失败 5 次后会被暂时限制 15 分钟
- **浏览器边界**：管理 API 默认只允许同源浏览器请求，并发送 CSP、防嵌入和 MIME 嗅探保护头
- **流量持久化**：每 60 秒刷入 SQLite，异常退出可能丢失最近一分钟计量
- **操作原子性**：站点创建/启停/更新如反代绑定失败，会回滚数据库并返回错误
- **优雅关闭**：收到 `SIGINT`/`SIGTERM` 后先 flush 流量再退出

---

## 验证 & CI/CD

```bash
go test -race ./...                                  # 运行测试和竞态检测
go vet ./...                                          # Go 静态检查
govulncheck ./...                                     # 已知漏洞检查
gosec -severity medium -confidence medium ./...       # 安全规则检查
go build -trimpath -buildvcs=false -o meridian .      # 编译
```

日常 push / pull request 会自动触发：

- 模块校验、竞态测试、`go vet`
- `govulncheck`、`gosec`、CodeQL
- 前端 JavaScript 与安装脚本语法检查
- 可复现路径裁剪构建

推送 `v*` 标签时自动触发：
- 多平台构建（linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64）
- 创建 GitHub Release 并上传二进制（包含 `meridian` 和 `meridian-relay`）
- 生成并上传 `SHA256SUMS`

---

## Roadmap

以下功能尚未实现，列在这里作为未来方向：

- [ ] 多用户 + 角色权限
- [ ] 审计日志
- [ ] Telegram / Webhook 通知

升级时建议优先保持这两样东西不变：

- `JWT_SECRET`
- SQLite 数据库文件及其同目录的 `-wal` / `-shm`

使用一键脚本执行 `update` 时，下列步骤会自动完成；手动部署时推荐：

1. 停止正在运行的 Meridian 服务。
2. 备份当前二进制、数据库文件和 `JWT_SECRET` 所在的环境配置。
3. 替换为新版本二进制或新镜像。
4. 用原来的 `JWT_SECRET` 和数据库重新启动。
5. 登录面板后检查站点列表、路径前缀和诊断页。

如果升级后临时忘记保留 `JWT_SECRET`，历史 JWT 会全部失效，表现为所有登录状态需要重新建立。

## 备份与恢复

一键脚本不再公开单独的备份命令。执行 `update` 或 `password` 时会自动短暂停止 systemd 服务并在 `/opt/meridian-backups` 创建一致性备份；如需自定义备份策略，请在停止 Meridian 后备份下列最小文件集。

最小备份集：

- `meridian.db`
- `meridian.db-wal`
- `meridian.db-shm`
- 保存 `JWT_SECRET` 的 `.env`、systemd 环境文件或容器环境配置

恢复步骤：

1. 停止 Meridian。
2. 还原数据库文件到原路径。
3. 还原原来的 `JWT_SECRET`。
4. 启动 Meridian。
5. 验证管理员登录、站点配置和代理路由。

---

## 限制与注意事项

- 当前只支持单管理员，不支持多用户或角色划分
- 没有审计日志，操作不可追溯
- 没有内置通知能力（无 Telegram / Webhook 集成）
- 管理面板本身不终止 TLS，公网部署必须放在 HTTPS 反向代理之后
- TLS 诊断会验证上游证书，但不负责证书签发和续期
- UA 诊断是本地配置预览，不验证远端实际收到的请求头
- 所有站点共享同一个面板端口，通过 URL 路径前缀（path_prefix）区分

## 开发须知

- 后端代码按职责拆分为 `internal/` 包：`auth`（JWT）、`db`（数据层）、`ua`（UA 改写）、`proxy`（代理引擎）、`diag`（诊断）、`router`（路由）、`server`（共享状态）、`handler_*`（API 处理器）
- 前端使用 hash 路由（`#dashboard`、`#sites`、`#access-logs`、`#access-analysis`），页面注册见 `web/static/js/app.js` 的 `Router.register`
- API 认证使用 JWT Bearer Token
- SQLite 驱动名为 `sqlite`（不是 `sqlite3`）
- 静态资源通过 `go:embed` 嵌入二进制

## 参与贡献

请参阅 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 安全问题

请参阅 [SECURITY.md](SECURITY.md)。

## License

MIT

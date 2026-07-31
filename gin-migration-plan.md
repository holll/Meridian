# Meridian 三项改动计划

## 背景

用户提出三个需求：清理 UI 中残留的端口字段、用 Gin 重写 API 层、调研 strm 302 代理方案。

---

## 改动一：清理 dashboard.js 残留端口字段

**问题**：Site 结构体已从 `listen_port` 改为 `path_prefix`，但 `dashboard.js` 两处仍引用旧字段，导致"端口"列显示 `undefined`。

### 修改文件

**`web/static/js/pages/dashboard.js`** — 2 处修改：

| 行号 | 旧代码 | 新代码 |
|------|--------|--------|
| 48 | `<th>端口</th>` | `<th>访问路径</th>` |
| 190 | `:${s.listen_port}` | `${esc(s.path_prefix)}` |

> sites.js 和 diag.js 中 4 处"端口"字样的文字描述不涉及数据模型字段（描述的是回源地址端口和面板端口），逻辑正确，不予修改。

### 验证

打开仪表盘页面，确认"访问路径"列显示站点 path_prefix（如 `/s/mysite`）而非空值。

---

## 改动二：Gin 重写 HTTP API 层

**目标**：将 HTTP 路由、中间件、handler 响应写入从 `net/http` 标准库迁移到 Gin 框架，handler 业务逻辑保持不变。

### 1. 依赖

`go.mod` 新增 `github.com/gin-gonic/gin v1.10.1`

### 2. 新增文件：`internal/router.go`（~90 行）

- `SetupRouter(app, pm, staticFS)` 函数：创建 `*gin.Engine`，注册全部路由和中间件
- `SecurityHeaders()` 全局中间件
- `CORS()` 全局中间件
- `AuthMiddleware()` 组级中间件
- `NoRoute` 兜底：`/api/*` 返回 404 → `pm.TryServe` → SPA

**路由表**：

| Gin 路由 | Handler |
|----------|---------|
| `POST /api/auth/setup` | HandleSetup |
| `POST /api/auth/login` | HandleLogin |
| `GET /api/auth/check` | HandleAuthCheck |
| `GET /api/dashboard` | HandleDashboard |
| `Any /api/sites` | HandleSites（内部 GET/POST dispatch）|
| `Any /api/sites/*action` | HandleSiteByID（内部 PUT/DELETE/toggle/diag dispatch）|
| `Any /api/traffic/*path` | HandleTraffic（`/overview` 或 `/{id}`）|
| `GET /api/ua-profiles` | HandleUAProfiles |
| `GET /api/events` | HandleSSE |

### 3. 修改文件：`internal/server.go`（约 -100 行）

**删除**（~50 行）：
- `func CORS()` → 迁移到 router.go
- `func SecurityHeaders()` → 迁移到 router.go
- `func (a *App) JSONResponse()` → 用 `c.JSON()` 替换
- `func (a *App) JSONOK()` → 用 `c.JSON()` 替换
- `func (a *App) JSONErr()` → 用 `c.JSON()` + `gin.H` 替换

**签名变更**：
- 所有 handler：`func (a *App) HandleXxx(w http.ResponseWriter, r *http.Request)` → `func (a *App) HandleXxx(c *gin.Context)`
- `decodeJSONBody(w, r, &req)` → `decodeJSONBody(c, &req)`
- `AuthMiddleware(next http.HandlerFunc)` → `AuthMiddleware() gin.HandlerFunc`
- `sendSSEEvent(w, flusher)` → `sendSSEEvent(c *gin.Context)`

**handler 内部变化**（机械替换）：
- `a.JSONErr(w, 500, "msg")` → `c.JSON(500, gin.H{"error": "msg"})`
- `a.JSONOK(w, data)` → `c.JSON(200, data)`
- `w.Header().Set("X", "Y")` → `c.Header("X", "Y")`
- `r.Method` → `c.Request.Method`
- `r.URL.Path` → `c.Request.URL.Path`（或 `c.Param("action")`）
- `r.URL.Query().Get("k")` → `c.Query("k")`

**不变的代码**：App 结构体、loginRateLimiter、requestClientKey、StaticHandler、startTime、所有 DB/ProxyManager 调用等业务逻辑。

### 4. 修改文件：`main.go`（~ -20 行）

路由注册从 50 行 `mux.HandleFunc(...)` 压缩为：
```go
router := internal.SetupRouter(app, pm, staticFS)
srv.Handler = router  // *gin.Engine 实现了 http.Handler
```

移除 main.go 中对 `"net/http"` 的 import（如仅用于路由的用法）。

### 5. 验证

```bash
go mod tidy && go build ./... && go vet ./... && go test ./...
```

所有 54 个现有测试必须通过（测试直接调用 handler 函数，Handler 签名变更后需更新测试中的调用）。

---

## 改动三：strm 302 重定向代理方案

### 现状

| 模式 | 行为 |
|------|------|
| **direct** | 透传 302 给客户端，客户端直连外部 URL，绕过代理 |
| **redirect** | 服务端跟随 302 到白名单内 URL，流量经代理（消耗带宽） |

**核心限制**：
- 不重写 `Location` 头——客户端要么直接访问外部 URL，要么全部流量经 Meridian
- 白名单必须预配置——.strm 指向的不在 `stream_hosts` 中的域名不被跟随
- redirect 模式放弃双上游路由
- 无日志记录重定向跟随/跳过

### 推荐方案：Location 重写（新增 `rewrite` 模式）

在现有 direct/redirect 之外增加第三种 `playback_mode: "rewrite"`：

**工作原理**：
1. Emby 返回 302 → `Location: https://cdn.example.com/video.mp4`
2. Meridian 拦截 302，不跟随，而是**重写 Location 头**指向自身代理路径
3. 重写后：`Location: https://panel.example.com/s/site1/__proxy__/https/cdn.example.com/video.mp4`
4. 客户端跟随这个重写后的 URL → 请求回到 Meridian
5. Meridian 识别 `__proxy__` 前缀，提取真实 URL 并代理内容

**优点**：
- 客户端始终通过 Meridian 访问，不感知外部源
- 支持流量计量和限速（所有内容经过代理）
- 不需要预配置白名单（按需代理任意域名的内容）

**缺点**：
- 带宽消耗与 redirect 模式相同（所有流量经 Meridian）
- URL 中暴露真实源地址（编码后可隐藏）

### 实现步骤（本次计划不执行，仅写方案）

1. `internal/db.go`：`PlaybackMode` 增加 `"rewrite"` 校验
2. `internal/proxy.go`：`StartSite` 中新增 `isRewriteMode` 分支
3. 新增 302 `Location` 头重写逻辑：将外部 URL 编码为 `{path_prefix}/__proxy__/{scheme}/{host}/{path}`
4. 新增 `__proxy__` 路径的代理处理：解码 URL 并做标准反代
5. `web/static/js/pages/sites.js`：播放模式下拉框增加 "URL 重写" 选项

本次计划**仅修复改动一和实施改动二**，改动三作为技术方案记录，待后续决定是否实施。

---

## 执行顺序

1. 清理 `dashboard.js` 残留端口字段（2 行修复）
2. `go.mod` 添加 gin 依赖
3. 创建 `internal/router.go`
4. 修改 `internal/server.go`（handler 签名 + 响应写入）
5. 修改 `main.go`（路由注册简化）
6. 更新测试
7. 运行 `go test ./...` + `go vet ./...`

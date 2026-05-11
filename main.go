package main

// api-docs.json 字段协议(api-kb 三方对齐,plan §A.4):
//
//   doc-writeback 阶段 AI 自动生成 modules/<name>/api-docs.json,平台
//   api-kb 在原有字段基础上增加两个:
//
//     - endpoint_key: "METHOD /path"(全大写 method + 单空格 + path)
//       项目内稳定锚点,贯穿 api-kb 的规则 / 反馈 / 沙盒 / AI 顾问。
//       缺这个字段时 admin-server SyncEndpointsForProject 会自动 backfill,
//       但建议 doc-writeback 直接输出避免 backfill。
//
//     - rules[]: 模块作者自报的"基础业务规则"
//       结构 {category, title, body, code_loc}。category 白名单:
//       business / flow / error / constraint / concurrency / order /
//       security / example。code_loc 形如 "main.go:120-145"。
//       平台对齐 job 会把这些规则与需求规格、技术方案融合,做三方溯源。
//       无规则时省略整个 rules 字段(不要输出空数组占位)。
//
//   完整示例:examples/api-docs.example.json

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// PluginContext 由 Runtime 提供的共享资源
//
// 字段协议（v3, 2026-05-04）：必须与 runtime/internal/plugin/interface.go 的字段
// 名/类型完全一致，Runtime 通过反射按字段名逐项赋值。
//
// 后台 worker 正确写法：
//
//	func (p *AdminAccountPlugin) Init(ctx PluginContext) error {
//	    p.lifecycleCtx = ctx.LifecycleCtx
//	    p.registerWorker = ctx.RegisterWorker
//	    go p.runTicker()
//	    return nil
//	}
//
//	func (p *AdminAccountPlugin) runTicker() {
//	    done := p.registerWorker()  // 告诉 Runtime 多一个活跃 worker
//	    defer done()                 // 退出时计数 -1
//	    t := time.NewTicker(time.Second)
//	    defer t.Stop()
//	    for {
//	        select {
//	        case <-p.lifecycleCtx.Done():
//	            return
//	        case <-t.C:
//	            // 干活
//	        }
//	    }
//	}
//
// WebSocket 相关字段（v3 新增）：
//   - Push / Emit / Broadcast / IsOnline: 模块给客户端主动发消息时调
//   - RegisterAuth: 仅登录模块在 Init 里调一次，把鉴权回调交给 runtime
type PluginContext struct {
	DB             *sql.DB
	Config         map[string]string
	Logger         *slog.Logger
	LifecycleCtx   context.Context
	RegisterWorker func() func() // goroutine 起来时调用，返回 dereg 函数供 defer
	IsUnloading    func() bool   // 查询模块是否处于 unloading（外部/定时任务可据此拒绝接新工作）

	// Push 给单个用户发"必送达"通知；模块业务里需要"通知用户"时调它
	// 入 ws_outbox 表后立即返回，后台 worker 投递，离线用户上线补发
	// data 任意可 json.Marshal 类型
	Push func(ctx context.Context, userID, code string, data any) (id int64, err error)

	// Emit 给单个用户发"尽力而为"事件，离线丢弃，不入库不重试
	// 适合实时性强但丢了无所谓的场景（聊天打字中、好友上线等）
	Emit func(userID, code string, data any) bool

	// Broadcast 给一组用户发"必送达"通知，每个用户独立 outbox 行
	Broadcast func(ctx context.Context, userIDs []string, code string, data any) (ids []int64, err error)

	// IsOnline 查询用户是否有在线 WebSocket 连接
	IsOnline func(userID string) bool

	// RegisterAuth 登录模块向 runtime 注册鉴权回调
	// 一个项目通常只有一个登录模块；普通业务模块这个字段保持 nil 即可
	//
	// verify(token) → 验证 access token 解析用户身份；ws 握手时调
	// refresh(refreshToken) → 用 refresh token 换新 access；自动续期定时器调
	// checkSession(userID, accessToken) → 巡检会话有效性；失效巡检定时器调
	RegisterAuth func(
		verify func(ctx context.Context, accessToken string) (userID string, expiresAt time.Time, refreshToken string, err error),
		refresh func(ctx context.Context, refreshToken string) (newAccess, newRefresh string, newExpiresAt time.Time, err error),
		checkSession func(ctx context.Context, userID, accessToken string) (valid bool, reason string),
	)
}

// Plugin 是导出的插件实例
// Runtime 通过 plugin.Lookup("Plugin") 加载此符号
// 符号名必须为 "Plugin"，类型必须实现 GamePlugin 接口
var Plugin = &AdminAccountPlugin{}

// Routes 声明本插件处理的所有 HTTP 路径
// Runtime 在 plugin.Lookup("Routes") 时读取这个 map，把所有路径注册到全局路由表
//
// 硬性约束：
//   - 必须是全局变量 var Routes = map[string]http.HandlerFunc{...}
//   - key 必须是编译期字面量字符串，不要用 fmt.Sprintf 拼接
//   - key 使用 Go 1.22+ 的 pattern 语法："METHOD /path" 或只写 "/path"
//   - value 必须是包级 handler 函数，不要写成方法值
//   - 禁止实现 ServeHTTP 方法，禁止在插件内部建 http.ServeMux
//
// 原因：Runtime 用全局 ServeMux 按路径精确分发请求。如果插件写
// ServeHTTP 或内部 mux，多个插件之间会互相拦截请求导致 404。
var Routes = map[string]http.HandlerFunc{
	"POST /api/admin-account/login":            handleLogin,
	"GET /api/admin-account/me":                handleMe,
	"POST /api/admin-account/create":           handleAccountCreate,
	"GET /api/admin-account/list":              handleAccountList,
	"GET /api/admin-account/detail":            handleAccountDetail,
	"PUT /api/admin-account/update":            handleAccountUpdate,
	"PUT /api/admin-account/reset-password":    handleResetPassword,
	"POST /api/admin-account/check-permission": handleCheckPermission,
	// 前台接口示例（以 /api/ 开头）
	"GET /api/admin_account/hello": handleHello,
	// 后台管理接口示例（以 /{admin_prefix}/api/ 开头，部署时替换为项目 UUID）
	"POST /{admin_prefix}/api/admin_account/admin/ping": handleAdminPing,
	// 注：内部自测端点 POST /_internal/selftest 由 selftest.go 在 init() 时
	// 注册进来，避免 var Routes 初始化循环依赖（selftest 需要回查 Routes）
}

// handleHello 是包级 handler，通过全局 Plugin 变量访问插件实例
func handleHello(w http.ResponseWriter, r *http.Request) {
	Plugin.handleHello(w, r)
}

func handleAdminPing(w http.ResponseWriter, r *http.Request) {
	Plugin.handleAdminPing(w, r)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	Plugin.handleLogin(w, r)
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	Plugin.handleMe(w, r)
}

func handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	Plugin.handleAccountCreate(w, r)
}

func handleAccountList(w http.ResponseWriter, r *http.Request) {
	Plugin.handleAccountList(w, r)
}

func handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	Plugin.handleAccountDetail(w, r)
}

func handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	Plugin.handleAccountUpdate(w, r)
}

func handleResetPassword(w http.ResponseWriter, r *http.Request) {
	Plugin.handleResetPassword(w, r)
}

func handleCheckPermission(w http.ResponseWriter, r *http.Request) {
	Plugin.handleCheckPermission(w, r)
}

// AdminAccountPlugin 实现 GamePlugin 接口
//
// 接口定义：
//
//	Name() string                        — 返回插件名称，必须与 plugin.yaml 中的 name 一致
//	Version() string                     — 返回插件版本，由 admin-server 在编译期通过 ldflags 注入；模块作者无需维护
//	Init(ctx PluginContext) error         — 插件初始化，接收 Runtime 共享资源
//	Shutdown(ctx context.Context) error  — 插件关闭，ctx 携带超时；后台 worker 应监听 LifecycleCtx.Done()
//
// 注意：不要在这个 struct 上添加 ServeHTTP 方法或 mux 字段。
// 所有 HTTP 路由通过顶层 Routes 全局变量声明。
type AdminAccountPlugin struct {
	db             *sql.DB
	rdb            *redis.Client
	logger         *slog.Logger
	lifecycleCtx   context.Context
	registerWorker func() func()
	isUnloading    func() bool
	mu             sync.Mutex
	accounts       map[string]accountRecord
	roles          map[string][]string
	sessionTTL     time.Duration
	adminAPIKey    string
	runtimeAddr    string

	// WebSocket 推送 API 缓存（来自 PluginContext，业务函数想主动给客户端发消息时调）
	push      func(ctx context.Context, userID, code string, data any) (int64, error)
	emit      func(userID, code string, data any) bool
	broadcast func(ctx context.Context, userIDs []string, code string, data any) ([]int64, error)
	isOnline  func(userID string) bool
}

// version 由 admin-server 在编译 .so 时通过 -ldflags "-X <module_path>.version=<deploy_tag>" 注入。
// 本地 go test 拿到的是 "dev"；线上 runtime 加载后调 Version() 拿到的是本次部署 tag。
var version = "dev"

func main() {}

func (p *AdminAccountPlugin) Name() string    { return "admin_account" }
func (p *AdminAccountPlugin) Version() string { return version }

func (p *AdminAccountPlugin) Init(ctx PluginContext) error {
	p.db = ctx.DB
	p.logger = ctx.Logger
	p.lifecycleCtx = ctx.LifecycleCtx
	p.registerWorker = ctx.RegisterWorker
	p.isUnloading = ctx.IsUnloading
	// WebSocket 推送 API 缓存到字段，业务函数随时取用
	p.push = ctx.Push
	p.emit = ctx.Emit
	p.broadcast = ctx.Broadcast
	p.isOnline = ctx.IsOnline
	p.sessionTTL = readSessionTTL(ctx.Config)
	p.adminAPIKey = ctx.Config["ADMIN_API_KEY"]
	p.runtimeAddr = ctx.Config["RUNTIME_ADDR"]
	p.rdb = newRedisClient(ctx.Config)
	// 仅登录模块需要：把鉴权回调注册给 runtime；普通业务模块这一行可删
	p.registerAuthIfLoginModule(ctx)
	// 建表、读 config；不要建 mux 或注册路由
	if err := p.initStorage(ctx.LifecycleCtx); err != nil {
		return err
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	p.logger.Info("插件初始化", "name", p.Name(), "version", p.Version())
	// 后台 worker 启动示例（见文件顶部 PluginContext 注释）：
	//   go p.runTicker()
	return nil
}

// Shutdown 插件优雅关闭
// ctx 携带超时（通常 3s）；LifecycleCtx 在 Reload 开始时已经 cancel，
// 后台 worker 应通过 lifecycleCtx.Done() 先行停止，无需在这里重复等待。
func (p *AdminAccountPlugin) Shutdown(ctx context.Context) error {
	fmt.Printf("[%s] 插件关闭\n", p.Name())
	// 如果有需要等待的资源（连接池 flush、文件关闭等），在 ctx.Done() 前完成：
	// select {
	// case <-ctx.Done():
	//     return ctx.Err()
	// case <-p.cleanupDone:
	//     return nil
	// }
	return nil
}

// handleHello 业务逻辑（方法接收者），由包级 handler 转发
func (p *AdminAccountPlugin) handleHello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, map[string]any{"module": p.Name(), "version": p.Version()}, "ok")
}

func (p *AdminAccountPlugin) handleAdminPing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, nil, "pong")
}

func readSessionTTL(config map[string]string) time.Duration {
	raw := "28800"
	if config != nil && config["ADMIN_ACCOUNTS_SESSION_TTL_SECONDS"] != "" {
		raw = config["ADMIN_ACCOUNTS_SESSION_TTL_SECONDS"]
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		seconds = 28800
	}
	return time.Duration(seconds) * time.Second
}

func newRedisClient(config map[string]string) *redis.Client {
	if config == nil {
		return nil
	}
	host := config["REDIS_HOST_LAN"]
	if host == "" {
		host = config["REDIS_HOST_WG"]
	}
	if host == "" {
		return nil
	}
	port := config["REDIS_PORT"]
	if port == "" {
		port = "6379"
	}
	db := 0
	if rawDB := config["REDIS_DB"]; rawDB != "" {
		if parsed, err := strconv.Atoi(rawDB); err == nil && parsed >= 0 {
			db = parsed
		}
	}
	return redis.NewClient(&redis.Options{
		Addr:     host + ":" + port,
		Password: config["REDIS_PASSWORD"],
		DB:       db,
	})
}

// ========== 用户认证工具 ==========

// getUserID 从请求中获取当前登录用户 ID
// 优先从 X-User-ID 头获取（Runtime 内部代理/测试调用时自动注入），
// 其次从 request context 获取（未来 Runtime 用户认证中间件注入）。
// 如果需要用户认证的接口，应使用此函数获取用户身份：
//
//	userID := getUserID(r)
//	if userID == "" {
//	    writeJSON(w, -1, nil, "未登录")  // 或 http.Error(w, ..., 401)
//	    return
//	}
func getUserID(r *http.Request) string {
	if uid := r.Header.Get("X-User-ID"); uid != "" {
		return uid
	}
	if uid, _ := r.Context().Value("user_id").(string); uid != "" {
		return uid
	}
	return ""
}

// writeJSON 统一响应格式：{ "status": 0, "data": ..., "msg": "..." }
func writeJSON(w http.ResponseWriter, status int, data any, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{"status": status, "data": data, "msg": msg}
	_ = json.NewEncoder(w).Encode(resp)
}

package main

// account_temporary_admin.go — 临时超管账号机制（task/inner_plugin.md §4.4 + §6）
//
// 设计意图：解决「鸡生蛋」问题 + 提供应急恢复
//   1. 项目里始终有一条 is_temporary=true 的种子记录，ID 永远不变
//   2. admin-server 通过 _create-temporary-admin 内部接口要求生成时，只重置
//      account / password_hash / status=enabled / expires_at，ID 不变
//   3. 同时只有一条临时超管记录（不会越长越大），重复调覆盖同一条
//   4. 10 分钟后过期，后台 worker 自动 status=disabled，记录保留下次复用
//
// 接口约束：
//   POST /api/account/_create-temporary-admin 必须带 X-Internal-Token header
//   外部访问无 token → 401，仅 admin-server 进程能调

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// 固定的临时超管账号 ID（跟 account_storage.go init 时的种子记录对齐）
// 12 字符 base62 短 ID，前 8 位 AccountT 独立可识别
const temporarySuperAdminSeedID = "AccountTmp01"

// 临时账号 TTL（10 分钟）
const temporaryAdminTTL = 10 * time.Minute

// handleCreateTemporaryAdmin POST /api/account/_create-temporary-admin
//
// admin-server 在「查看超管账号密码」按钮 + 项目部署完成事件里调用此接口。
// 接口重置种子记录的 account / password_hash / status / expires_at，返回新账号 + 明文密码。
func (p *AdminAccountPlugin) handleCreateTemporaryAdmin(w http.ResponseWriter, r *http.Request) {
	// 鉴权：必须带 X-Internal-Token，并跟模块自己掌握的 token 匹配
	got := r.Header.Get("X-Internal-Token")
	want := strings.TrimSpace(os.Getenv("ADMIN_API_KEY"))
	if want == "" {
		// 兜底：ADMIN_API_KEY 没注入时也允许 RUNTIME_INTERNAL_TOKEN 兜底（开发态）
		want = strings.TrimSpace(os.Getenv("RUNTIME_INTERNAL_TOKEN"))
	}
	if got == "" || want == "" || got != want {
		writeJSON(w, 2401, nil, "未授权访问内部接口")
		return
	}

	ctx := r.Context()
	if p.db == nil {
		writeJSON(w, 2402, nil, "数据库未就绪")
		return
	}

	// 生成 8 位随机账号 + 16 位随机密码（明文返回给运维一次，不存）
	newAccount, err := randomHex(4) // 8 位 hex
	if err != nil {
		writeJSON(w, 2403, nil, "生成账号失败")
		return
	}
	plainPassword, err := randomHex(8) // 16 位 hex
	if err != nil {
		writeJSON(w, 2403, nil, "生成密码失败")
		return
	}
	// 复用模块原生 hashPassword（sha256 hex），跟 verifyPassword 兼容
	passwordHash, err := hashPassword(plainPassword)
	if err != nil {
		writeJSON(w, 2403, nil, "密码加密失败")
		return
	}

	expiresAt := time.Now().Add(temporaryAdminTTL)

	// 重置种子记录的字段，ID 永远不变
	// status 从可能的 disabled 解开为 enabled
	// account 字段加随机后缀防 UNIQUE 冲突（旧值在表里，新值覆盖必须不冲突）
	_, err = p.db.ExecContext(ctx, `
		UPDATE account_accounts
		   SET account = $1,
		       password_hash = $2,
		       status = 'enabled',
		       expires_at = $3,
		       updated_at = now()
		 WHERE id = $4
	`, newAccount, passwordHash, expiresAt, temporarySuperAdminSeedID)
	if err != nil {
		writeJSON(w, 2404, nil, "更新临时超管账号失败: "+err.Error())
		return
	}

	writeJSON(w, 0, map[string]any{
		"account":     newAccount,
		"password":    plainPassword,
		"expires_at":  expiresAt.Format(time.RFC3339),
		"ttl_seconds": int(temporaryAdminTTL.Seconds()),
	}, "ok")
}

// startTemporaryAdminWorker 启动后台 worker，每分钟扫一次过期的临时超管账号
//
// 过期判定：is_temporary=true 且 status=enabled 且 expires_at < now()
// 处理：置 status=disabled，**不删记录、不改 account 字段**（保留下次按钮覆盖时复用）
//
// 调用方：Init() 里 go p.startTemporaryAdminWorker(ctx)
func (p *AdminAccountPlugin) startTemporaryAdminWorker(ctx context.Context) {
	done := func() {}
	if p.registerWorker != nil {
		done = p.registerWorker()
	}
	defer done()

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.db == nil {
				continue
			}
			_, err := p.db.ExecContext(ctx, `
				UPDATE account_accounts
				   SET status = 'disabled', updated_at = now()
				 WHERE is_temporary = TRUE
				   AND status = 'enabled'
				   AND expires_at IS NOT NULL
				   AND expires_at < now()
			`)
			if err != nil && p.logger != nil {
				p.logger.Warn("临时超管过期扫描失败", "err", err.Error())
			}
		}
	}
}

// randomHex 生成 n 字节的随机 hex 字符串（输出长度 2n）
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// 编译期防止 json/encoding 未引用警告
var _ = json.Marshal

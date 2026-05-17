package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var errAccountNotFound = errors.New("account not found")

func (p *AdminAccountPlugin) initStorage(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.db == nil {
		p.ensureMemoryStore()
		return nil
	}
	for _, stmt := range []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		// 平台短 ID 标准 12 字符 base62, generate_short_id() 由 runtime migration11 注入
		`CREATE TABLE IF NOT EXISTS account_accounts (
			id TEXT PRIMARY KEY DEFAULT generate_short_id(),
			account TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'enabled',
			is_super_admin BOOLEAN NOT NULL DEFAULT FALSE,
			is_temporary BOOLEAN NOT NULL DEFAULT FALSE,
			expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS account_role_bindings (
			id TEXT PRIMARY KEY DEFAULT generate_short_id(),
			account_id TEXT NOT NULL REFERENCES account_accounts(id) ON DELETE CASCADE,
			role_id TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (account_id, role_id)
		)`,
		// 临时超管账号种子记录（task/inner_plugin.md §4.4 § §6.1）
		// 项目级唯一一条 is_temporary=true 的记录，初始 disabled
		// admin-server 调 _create-temporary-admin 接口时只重置 account/password_hash/status/expires_at，ID 永远不变
		`INSERT INTO account_accounts (id, account, password_hash, status, is_super_admin, is_temporary)
		 VALUES ('AccTmpAdm001', '__temporary_super_admin_seed__', '', 'disabled', TRUE, TRUE)
		 ON CONFLICT (id) DO NOTHING`,
	} {
		if _, err := p.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (p *AdminAccountPlugin) ensureMemoryStore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accounts == nil {
		p.accounts = map[string]accountRecord{}
	}
	if p.roles == nil {
		p.roles = map[string][]string{}
	}
}

func (p *AdminAccountPlugin) getAccountByID(ctx context.Context, accountID string) (accountRecord, []string, bool, error) {
	if p.db != nil {
		acc, ok, err := p.queryAccount(ctx, "id=$1", accountID)
		if err != nil || !ok {
			return acc, nil, ok, err
		}
		roles, err := p.accountRoleIDs(ctx, acc.ID)
		return acc, roles, true, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, ok := p.accounts[accountID]
	return acc, append([]string(nil), p.roles[accountID]...), ok, nil
}

func (p *AdminAccountPlugin) getAccountByAccount(ctx context.Context, account string) (accountRecord, []string, bool, error) {
	if p.db != nil {
		acc, ok, err := p.queryAccount(ctx, "account=$1", account)
		if err != nil || !ok {
			return acc, nil, ok, err
		}
		roles, err := p.accountRoleIDs(ctx, acc.ID)
		return acc, roles, true, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, acc := range p.accounts {
		if acc.Username == account {
			return acc, append([]string(nil), p.roles[acc.ID]...), true, nil
		}
	}
	return accountRecord{}, nil, false, nil
}

func (p *AdminAccountPlugin) queryAccount(ctx context.Context, where string, arg any) (accountRecord, bool, error) {
	var acc accountRecord
	err := p.db.QueryRowContext(ctx, `
		SELECT id, account, password_hash, status, is_super_admin, created_at, updated_at
		FROM account_accounts WHERE `+where, arg).
		Scan(&acc.ID, &acc.Username, &acc.PasswordHash, &acc.Status, &acc.IsSuperAdmin, &acc.CreatedAt, &acc.UpdatedAt)
	if sqlNoRows(err) {
		return accountRecord{}, false, nil
	}
	if err != nil {
		return accountRecord{}, false, err
	}
	return acc, true, nil
}

func (p *AdminAccountPlugin) accountRoleIDs(ctx context.Context, accountID string) ([]string, error) {
	if p.db == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return append([]string(nil), p.roles[accountID]...), nil
	}
	rows, err := p.db.QueryContext(ctx, "SELECT role_id FROM account_role_bindings WHERE account_id=$1 ORDER BY role_id", accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (p *AdminAccountPlugin) createAccount(ctx context.Context, account, password, status string, roleIDs []string) (accountRecord, []string, error) {
	now := time.Now().UTC()
	passwordHash, err := hashPassword(password)
	if err != nil {
		return accountRecord{}, nil, err
	}
	if p.db != nil {
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return accountRecord{}, nil, err
		}
		defer tx.Rollback()

		var acc accountRecord
		err = tx.QueryRowContext(ctx, `
			INSERT INTO account_accounts (account, password_hash, status, is_super_admin)
			VALUES ($1, $2, $3, FALSE)
			RETURNING id, account, password_hash, status, is_super_admin, created_at, updated_at`,
			account, passwordHash, status).
			Scan(&acc.ID, &acc.Username, &acc.PasswordHash, &acc.Status, &acc.IsSuperAdmin, &acc.CreatedAt, &acc.UpdatedAt)
		if err != nil {
			return accountRecord{}, nil, err
		}
		if err := p.replaceAccountRolesTx(ctx, tx, acc.ID, roleIDs); err != nil {
			return accountRecord{}, nil, err
		}
		if err := tx.Commit(); err != nil {
			return accountRecord{}, nil, err
		}
		return acc, roleIDs, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	acc := accountRecord{ID: newUUIDLikeID(), Username: account, PasswordHash: passwordHash, Status: status, CreatedAt: now, UpdatedAt: now}
	p.accounts[acc.ID] = acc
	p.roles[acc.ID] = append([]string(nil), roleIDs...)
	return acc, roleIDs, nil
}

func (p *AdminAccountPlugin) listAccounts(ctx context.Context, status, keyword string, page, pageSize int) ([]accountResponse, int, error) {
	if p.db != nil {
		where, args := "TRUE", []any{}
		if status != "" {
			args = append(args, status)
			where += " AND status=$" + strconv.Itoa(len(args))
		}
		if keyword != "" {
			args = append(args, "%"+keyword+"%")
			where += " AND lower(account) LIKE $" + strconv.Itoa(len(args))
		}
		var total int
		if err := p.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_accounts WHERE "+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		args = append(args, pageSize, (page-1)*pageSize)
		rows, err := p.db.QueryContext(ctx, `
			SELECT id, account, password_hash, status, is_super_admin, created_at, updated_at
			FROM account_accounts WHERE `+where+` ORDER BY created_at DESC LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		items := []accountResponse{}
		for rows.Next() {
			var acc accountRecord
			if err := rows.Scan(&acc.ID, &acc.Username, &acc.PasswordHash, &acc.Status, &acc.IsSuperAdmin, &acc.CreatedAt, &acc.UpdatedAt); err != nil {
				return nil, 0, err
			}
			roles, err := p.accountRoleIDs(ctx, acc.ID)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, accountToResponse(acc, roles))
		}
		return items, total, rows.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	all := make([]accountResponse, 0, len(p.accounts))
	for _, acc := range p.accounts {
		if status != "" && acc.Status != status {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(acc.Username), keyword) {
			continue
		}
		all = append(all, accountToResponse(acc, p.roles[acc.ID]))
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	total := len(all)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return all[start:end], total, nil
}

func (p *AdminAccountPlugin) updateAccount(ctx context.Context, accountID, status string, roleIDs []string) (accountRecord, []string, bool, error) {
	if p.db != nil {
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return accountRecord{}, nil, false, err
		}
		defer tx.Rollback()

		var acc accountRecord
		err = tx.QueryRowContext(ctx, `
			UPDATE account_accounts SET status=$2, updated_at=now()
			WHERE id=$1
			RETURNING id, account, password_hash, status, is_super_admin, created_at, updated_at`, accountID, status).
			Scan(&acc.ID, &acc.Username, &acc.PasswordHash, &acc.Status, &acc.IsSuperAdmin, &acc.CreatedAt, &acc.UpdatedAt)
		if sqlNoRows(err) {
			return accountRecord{}, nil, false, nil
		}
		if err != nil {
			return accountRecord{}, nil, false, err
		}
		if err := p.replaceAccountRolesTx(ctx, tx, acc.ID, roleIDs); err != nil {
			return accountRecord{}, nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return accountRecord{}, nil, false, err
		}
		return acc, roleIDs, true, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, ok := p.accounts[accountID]
	if !ok {
		return accountRecord{}, nil, false, nil
	}
	acc.Status = status
	acc.UpdatedAt = time.Now().UTC()
	p.accounts[acc.ID] = acc
	p.roles[acc.ID] = append([]string(nil), roleIDs...)
	return acc, roleIDs, true, nil
}

func (p *AdminAccountPlugin) replaceAccountRoles(ctx context.Context, accountID string, roleIDs []string) error {
	if p.db == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.roles[accountID] = append([]string(nil), roleIDs...)
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := p.replaceAccountRolesTx(ctx, tx, accountID, roleIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *AdminAccountPlugin) replaceAccountRolesTx(ctx context.Context, tx *sql.Tx, accountID string, roleIDs []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_role_bindings WHERE account_id=$1", accountID); err != nil {
		return err
	}
	for _, roleID := range roleIDs {
		if _, err := tx.ExecContext(ctx, "INSERT INTO account_role_bindings (account_id, role_id) VALUES ($1, $2)", accountID, roleID); err != nil {
			return err
		}
	}
	return nil
}

func (p *AdminAccountPlugin) resetPassword(ctx context.Context, accountID, newPassword string) (accountRecord, bool, error) {
	passwordHash, err := hashPassword(newPassword)
	if err != nil {
		return accountRecord{}, false, err
	}
	if p.db != nil {
		var acc accountRecord
		err := p.db.QueryRowContext(ctx, `
			UPDATE account_accounts SET password_hash=$2, updated_at=now()
			WHERE id=$1
			RETURNING id, account, password_hash, status, is_super_admin, created_at, updated_at`,
			accountID, passwordHash).
			Scan(&acc.ID, &acc.Username, &acc.PasswordHash, &acc.Status, &acc.IsSuperAdmin, &acc.CreatedAt, &acc.UpdatedAt)
		if sqlNoRows(err) {
			return accountRecord{}, false, nil
		}
		return acc, err == nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, ok := p.accounts[accountID]
	if !ok {
		return accountRecord{}, false, nil
	}
	acc.PasswordHash = passwordHash
	acc.UpdatedAt = time.Now().UTC()
	p.accounts[acc.ID] = acc
	return acc, true, nil
}

func (p *AdminAccountPlugin) createSession(ctx context.Context, accountID string) (string, time.Time, error) {
	token := newToken()
	expiresAt := time.Now().UTC().Add(p.sessionTTL)
	if p.rdb == nil {
		return "", time.Time{}, errors.New("redis unavailable")
	}
	key := sessionRedisKey(token)
	if err := p.rdb.HSet(ctx, key, map[string]any{
		"account_id": accountID,
		"expires_at": expiresAt.Format(time.RFC3339),
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}).Err(); err != nil {
		return "", time.Time{}, err
	}
	if err := p.rdb.Expire(ctx, key, p.sessionTTL).Err(); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (p *AdminAccountPlugin) getAccountBySession(ctx context.Context, token string) (accountRecord, []string, bool, error) {
	if p.rdb == nil {
		return accountRecord{}, nil, false, nil
	}
	accountID, err := p.rdb.HGet(ctx, sessionRedisKey(token), "account_id").Result()
	if errors.Is(err, redis.Nil) {
		return accountRecord{}, nil, false, nil
	}
	if err != nil {
		return accountRecord{}, nil, false, err
	}
	return p.getAccountByID(ctx, accountID)
}

func (p *AdminAccountPlugin) getAccountBySessionState(ctx context.Context, token string) (accountRecord, []string, sessionAccountState, error) {
	if p.rdb == nil {
		return accountRecord{}, nil, sessionMissing, nil
	}
	accountID, err := p.rdb.HGet(ctx, sessionRedisKey(token), "account_id").Result()
	if errors.Is(err, redis.Nil) {
		return accountRecord{}, nil, sessionMissing, nil
	}
	if err != nil {
		return accountRecord{}, nil, sessionMissing, err
	}
	acc, roles, ok, err := p.getAccountByID(ctx, accountID)
	if err != nil {
		return accountRecord{}, nil, sessionMissing, err
	}
	if !ok {
		return accountRecord{}, nil, sessionAccountMissing, nil
	}
	return acc, roles, sessionAccountOK, nil
}

func sessionRedisKey(token string) string {
	return "admin_accounts:session:" + token
}

func hashPassword(password string) (string, error) {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:]), nil
}

func verifyPassword(hash, password string) bool {
	expected, err := hashPassword(password)
	return err == nil && hash == expected
}

func accountToResponse(acc accountRecord, roles []string) accountResponse {
	return accountResponse{
		AccountID: acc.ID, Account: acc.Username,
		Status: acc.Status, RoleIDs: append([]string(nil), roles...), IsSuperAdmin: acc.IsSuperAdmin,
		CreatedAt: acc.CreatedAt.Format(time.RFC3339), UpdatedAt: acc.UpdatedAt.Format(time.RFC3339),
	}
}

func normalizeStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "enabled" || status == "disabled" {
		return status
	}
	return ""
}

func sqlNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

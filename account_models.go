package main

import "time"

type accountRecord struct {
	ID           string
	Username     string
	PasswordHash string
	DisplayName  string
	Status       string
	IsSuperAdmin bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type sessionRecord struct {
	AccountID    string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type accountRoleResponse struct {
	RoleID     string `json:"role_id"`
	RoleStatus string `json:"role_status"`
}

type accountResponse struct {
	AccountID    string   `json:"account_id"`
	Account      string   `json:"account"`
	Status       string   `json:"status"`
	RoleIDs      []string `json:"role_ids"`
	IsSuperAdmin bool     `json:"is_super_admin"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// 平台短 ID 标准 12 字符 base62, 语义前缀让 UI 缩短显示也能区分
// rootRoleID / rootPermID / supportRoleID 必须跟 role/permission 公共模块对齐
const (
	rootAccountID     = "AccRoot00001"
	rootUsername      = "root"
	operatorAccountID = "AccOper00001"
	rootRoleID        = "RoleRoot0001"
	supportRoleID     = "RoleSupp0001"
	rootPermID        = "PermSysMan01"
)

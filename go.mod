module github.com/oh-my-porjects/myanmarGames---admin_account

go 1.25

// 强制锁定 Go 工具链版本（含 patch 号），跟 runtime 镜像、admin-server 编模块器字节级一致
// Go plugin 机制要求 .so 跟主进程用完全相同的工具链，差一个 patch 都会拒绝加载
// 真相源在 omps-dev-workspace 根的 GO_VERSION 文件，用户项目从模板继承后不要手改
toolchain go1.25.10

require (
	github.com/redis/go-redis/v9 v9.7.0
	golang.org/x/crypto v0.17.0
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
)

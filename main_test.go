package main

import (
	"context"
	"log/slog"
	"testing"
)

func TestPluginName(t *testing.T) {
	name := Plugin.Name()
	if name == "" {
		t.Fatal("Name() 不应为空")
	}
}

func TestPluginInit(t *testing.T) {
	ctx := PluginContext{
		Logger: slog.Default(),
	}
	if err := Plugin.Init(ctx); err != nil {
		t.Errorf("Init 失败: %v", err)
	}
}

func TestPluginShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	if err := Plugin.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown(ctx) 失败: %v", err)
	}
}

package server

import (
	"testing"
	"time"
)

// clearDynamicModelsCache 清空包级动态模型目录缓存：正缓存（ids/fetched）与
// 失败负缓存（lastFail）一并清。
//
// 为什么需要：dynamicModelsCache 是包级全局，跨用例共享。任一用例写入
// lastFail 后，后续用例在 5min 冷却窗口内拿到的就是空列表——表现为
// `go test -shuffle=N` 随机失败（TestModelsEndpoint: models count=0）。
// 生产路径不变，只在测试侧提供重置入口。
func clearDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// resetDynamicModelsCache 用例入口清零 + t.Cleanup 再清一次：进入时不受前序
// 用例污染，退出时不把本用例写入的正/负缓存外溢给后续用例。
func resetDynamicModelsCache(t *testing.T) {
	t.Helper()
	clearDynamicModelsCache()
	t.Cleanup(clearDynamicModelsCache)
}

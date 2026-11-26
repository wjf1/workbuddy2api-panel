// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// VerifyAny 校验 Bearer 或 x-api-key 两种凭证形态：OpenAI 系客户端用
// `Authorization: Bearer <key>`；Anthropic 协议客户端（Claude Code、ZCode 的
// anthropic 供应商）用 `x-api-key: <key>`。任一命中即通过。
//
// key 为空表示"未启用鉴权"，恒返回 true。两种形态都走常量时间比较；凭证缺失
// 时同样做一次摘要比较，保持耗时形状一致（与 VerifyBearer 同哲学）。
func VerifyAny(r *http.Request, key string) bool {
	ok, _ := VerifyAnyGroup(r, key, nil)
	return ok
}

// VerifyAnyGroup 与 VerifyAny 同校验逻辑，但支持多密钥（密钥分组分发）：
// keys 为 nil/空时退化为单一 key（零行为差异，VerifyAny 的直接调用方）。
// 命中时返回该密钥在 keys 中的下标（单 key 恒 0）；未命中/未启用时返回 -1。
// 每个候选密钥独立做常量时间比较，与单 key 路径保持同一耗时形状哲学。
func VerifyAnyGroup(r *http.Request, key string, keys []string) (bool, int) {
	if len(keys) == 0 {
		return verifyAnySingle(r, key), 0
	}
	tok, has := credentialToken(r)
	if !has {
		// 凭证缺失：对每个候选做一次摘要比较，保持耗时形状一致。
		for _, k := range keys {
			subtle.ConstantTimeCompare(digest(""), digest(k))
		}
		return false, -1
	}
	for i, k := range keys {
		if k == "" {
			continue
		}
		if subtle.ConstantTimeCompare(digest(tok), digest(k)) == 1 {
			return true, i
		}
	}
	return false, -1
}

// verifyAnySingle 单密钥路径（VerifyAny 原实现，经分组版重构后保留独立函数：
// keys 为空时零循环开销，也供 VerifyBearer 的语义对称）。
func verifyAnySingle(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), bearerPrefix) {
		return VerifyBearer(r, key)
	}
	tok := r.Header.Get("x-api-key")
	if tok == "" {
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// credentialToken 取出请求的凭证原文（Bearer token 或 x-api-key）与是否存在。
func credentialToken(r *http.Request) (string, bool) {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, bearerPrefix) {
		return authz[len(bearerPrefix):], true
	}
	if tok := r.Header.Get("x-api-key"); tok != "" {
		return tok, true
	}
	return "", false
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

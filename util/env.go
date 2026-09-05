package util

import "os"

// EnvOrDefault 读取环境变量,未设置或为空时返回 fallback。
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── N3(2026-09-05):上游轮换敏感的硬编码指纹外置 ──
//
// sentinel SDK 版本 / Oai-Client-Build-Number 此前散落在 6 个文件里硬编码,
// 上游轮换(实测约每月)后求解全部失败,必须改代码重编译重部署。
// 外置为环境变量后,上游轮换时仅需改 compose 环境变量重启容器即可恢复。

const (
	defaultSentinelSV    = "20260423af3c"
	defaultOaiBuildNum   = "7823760"
	sentinelSDKURLPrefix = "https://chatgpt.com/sentinel/"
	sentinelSDKURLSuffix = "/sdk.js"
)

// SentinelSDKVersion 返回 sentinel SDK 版本(环境变量 SENTINEL_SDK_VERSION,
// 默认 20260423af3c)。用于 prooftoken / so / turnstile 求解与 sentinel frame Referer。
func SentinelSDKVersion() string {
	return EnvOrDefault("SENTINEL_SDK_VERSION", defaultSentinelSV)
}

// SentinelSDKURL 返回版本化 sentinel SDK 脚本 URL。
func SentinelSDKURL() string {
	return sentinelSDKURLPrefix + SentinelSDKVersion() + sentinelSDKURLSuffix
}

// OaiBuildNumber 返回 Oai-Client-Build-Number(环境变量 OAI_BUILD_NUMBER,
// 默认 7823760)。
func OaiBuildNumber() string {
	return EnvOrDefault("OAI_BUILD_NUMBER", defaultOaiBuildNum)
}

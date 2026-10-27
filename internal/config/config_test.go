package config

import (
	"os"
	"reflect"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// 清除所有相关环境变量
	for _, key := range []string{"SERVER_HOST", "SERVER_PORT", "PORT", "STREAM_MODE", "MAX_CONTINUE_COUNT"} {
		os.Unsetenv(key)
	}

	cfg := Load()

	if cfg.ServerHost != "0.0.0.0" {
		t.Errorf("ServerHost = %q, want %q", cfg.ServerHost, "0.0.0.0")
	}
	if cfg.ServerPort != "8080" {
		t.Errorf("ServerPort = %q, want %q", cfg.ServerPort, "8080")
	}
	if !cfg.StreamMode {
		t.Error("StreamMode = false, want true")
	}
	if cfg.MaxContinueCount != 3 {
		t.Errorf("MaxContinueCount = %d, want 3", cfg.MaxContinueCount)
	}
}

func TestLoadWithEnv(t *testing.T) {
	os.Setenv("SERVER_HOST", "127.0.0.1")
	os.Setenv("SERVER_PORT", "9090")
	os.Setenv("STREAM_MODE", "false")
	defer func() {
		os.Unsetenv("SERVER_HOST")
		os.Unsetenv("SERVER_PORT")
		os.Unsetenv("STREAM_MODE")
	}()

	cfg := Load()

	if cfg.ServerHost != "127.0.0.1" {
		t.Errorf("ServerHost = %q, want %q", cfg.ServerHost, "127.0.0.1")
	}
	if cfg.ServerPort != "9090" {
		t.Errorf("ServerPort = %q, want %q", cfg.ServerPort, "9090")
	}
	if cfg.StreamMode {
		t.Error("StreamMode = true, want false")
	}
}

func TestGetBoolEnvInvalid(t *testing.T) {
	os.Setenv("TEST_BOOL", "notabool")
	defer os.Unsetenv("TEST_BOOL")

	result := getBoolEnv("TEST_BOOL", true)
	if !result {
		t.Error("getBoolEnv with invalid value should return default (true)")
	}
}

// ticket 05:expert 档会话复用开关,默认开启(live 验证 expert 续轮正常,
// 开关仅备而不用);置 0/false 时 expert 退回每轮新开。
func TestDeepSeekExpertResumeFlag(t *testing.T) {
	t.Setenv("DEEPSEEK_EXPERT_RESUME", "")
	if !Load().DeepSeekExpertResume {
		t.Error("默认应为 true(expert 复用开启)")
	}
	t.Setenv("DEEPSEEK_EXPERT_RESUME", "0")
	if Load().DeepSeekExpertResume {
		t.Error("DEEPSEEK_EXPERT_RESUME=0 应关闭 expert 复用")
	}
	t.Setenv("DEEPSEEK_EXPERT_RESUME", "true")
	if !Load().DeepSeekExpertResume {
		t.Error("DEEPSEEK_EXPERT_RESUME=true 应开启")
	}
}

// ticket 01:provider 工具调用两组开关必须默认全关/全空——新能力不能隐式启用。
func TestProviderToolDefaults(t *testing.T) {
	for _, key := range []string{
		"PROVIDER_TOOL_EMULATION", "PROVIDER_TOOL_MODELS", "PROVIDER_TOOL_RETRIES",
		"NATIVE_TOOLS_BASE", "NATIVE_TOOLS_KEY", "NATIVE_TOOLS_MODEL_MAP",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()

	if cfg.ProviderToolEmulation {
		t.Error("ProviderToolEmulation 默认应为 false")
	}
	if cfg.ProviderToolModels != nil {
		t.Errorf("ProviderToolModels 默认应为空,得到 %v", cfg.ProviderToolModels)
	}
	if cfg.ProviderToolRetries != 3 {
		t.Errorf("ProviderToolRetries 默认应为 3,得到 %d", cfg.ProviderToolRetries)
	}
	if cfg.NativeToolsBase != "" {
		t.Errorf("NativeToolsBase 默认应为空,得到 %q", cfg.NativeToolsBase)
	}
	if cfg.NativeToolsKey != "" {
		t.Errorf("NativeToolsKey 默认应为空,得到 %q", cfg.NativeToolsKey)
	}
	if cfg.NativeToolsModelMap != nil {
		t.Errorf("NativeToolsModelMap 默认应为空,得到 %v", cfg.NativeToolsModelMap)
	}
}

// ticket 01:PROVIDER_TOOL_RETRIES 沿用 REFUSAL_RETRIES 语义——
// 0 是合法值(表示不重试),非法值(abc/负数)回落默认 3。
func TestProviderToolRetries(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"默认", "", 3},
		{"零=不重试", "0", 0},
		{"正常整数", "5", 5},
		{"非数字回落默认", "abc", 3},
		{"负数回落默认", "-1", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROVIDER_TOOL_RETRIES", tc.env)
			if got := Load().ProviderToolRetries; got != tc.want {
				t.Errorf("PROVIDER_TOOL_RETRIES=%q 得到 %d,想要 %d", tc.env, got, tc.want)
			}
		})
	}
}

// ticket 01:布尔开关走 getBoolEnv,非法值回落默认 false。
func TestProviderToolEmulationFlag(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"默认关闭", "", false},
		{"true 开启", "true", true},
		{"1 开启", "1", true},
		{"false 关闭", "false", false},
		{"非法回落默认", "yes-please", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROVIDER_TOOL_EMULATION", tc.env)
			if got := Load().ProviderToolEmulation; got != tc.want {
				t.Errorf("PROVIDER_TOOL_EMULATION=%q 得到 %v,想要 %v", tc.env, got, tc.want)
			}
		})
	}
}

// ticket 01:两个 CSV 字段复用 splitCSV——裁空白、丢空项;空串返回 nil。
// NATIVE_TOOLS_MODEL_MAP 保留原始 "<客户端>=<上游>" 字符串,解析留给 provider 侧。
func TestProviderToolCSV(t *testing.T) {
	cases := []struct {
		name       string
		models     string
		modelMap   string
		wantModels []string
		wantMap    []string
	}{
		{
			name:       "空串返回 nil",
			models:     "",
			modelMap:   "",
			wantModels: nil,
			wantMap:    nil,
		},
		{
			name:       "裁剪空白并丢弃空项",
			models:     " deepseek-chat , glm-4 ,, kimi-k2 ",
			modelMap:   " a=up-a , b=up-b ,,",
			wantModels: []string{"deepseek-chat", "glm-4", "kimi-k2"},
			wantMap:    []string{"a=up-a", "b=up-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROVIDER_TOOL_MODELS", tc.models)
			t.Setenv("NATIVE_TOOLS_MODEL_MAP", tc.modelMap)

			cfg := Load()

			if !reflect.DeepEqual(cfg.ProviderToolModels, tc.wantModels) {
				t.Errorf("ProviderToolModels = %#v, 想要 %#v", cfg.ProviderToolModels, tc.wantModels)
			}
			if !reflect.DeepEqual(cfg.NativeToolsModelMap, tc.wantMap) {
				t.Errorf("NativeToolsModelMap = %#v, 想要 %#v", cfg.NativeToolsModelMap, tc.wantMap)
			}
		})
	}
}

// ticket 01:方案 B 的 base/key 是普通字符串读取,不做默认值。
func TestNativeToolsBaseAndKey(t *testing.T) {
	t.Setenv("NATIVE_TOOLS_BASE", "https://api.deepseek.com/v1")
	t.Setenv("NATIVE_TOOLS_KEY", "sk-test")

	cfg := Load()

	if cfg.NativeToolsBase != "https://api.deepseek.com/v1" {
		t.Errorf("NativeToolsBase = %q", cfg.NativeToolsBase)
	}
	if cfg.NativeToolsKey != "sk-test" {
		t.Errorf("NativeToolsKey = %q", cfg.NativeToolsKey)
	}
}

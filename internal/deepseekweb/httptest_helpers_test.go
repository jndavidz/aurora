package deepseekweb

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// writeJSON 输出 JSON 响应(测试辅助)。
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// readJSONBody 读取并解析请求体 JSON(测试辅助)。
func readJSONBody(t *testing.T, r *http.Request, v any) error {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

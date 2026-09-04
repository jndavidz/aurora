package apierrors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest("POST", "/test", nil)
	return c, w
}

func TestInvalidRequest(t *testing.T) {
	c, w := setupTestContext()
	InvalidRequest(c, "bad input", "test_error")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty response body")
	}
}

func TestMissingParam(t *testing.T) {
	c, w := setupTestContext()
	MissingParam(c, "prompt", "missing_required_parameter")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAuthError(t *testing.T) {
	c, w := setupTestContext()
	AuthError(c, http.StatusUnauthorized, "invalid token")

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestInternalError(t *testing.T) {
	c, w := setupTestContext()
	InternalError(c, "server_error", "something broke", 500)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

func TestBadRequest(t *testing.T) {
	c, w := setupTestContext()
	BadRequest(c, "invalid_type", "bad request", "test_code")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestNotFoundAccount(t *testing.T) {
	c, w := setupTestContext()
	NotFoundAccount(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

// E2 回归:MissingParam 曾双重写入(先 InvalidRequest 再 JSONError),
// 客户端收到两个拼接的 JSON。修复后应只有一次 c.JSON。
func TestMissingParamSingleWrite(t *testing.T) {
	c, w := setupTestContext()
	MissingParam(c, "messages", "missing_required_parameter")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	body := w.Body.String()
	if n := strings.Count(body, `"message"`); n != 1 {
		t.Errorf("response body 应只有一个 message 字段(实际 %d): %s", n, body)
	}
	if !strings.Contains(body, `"param":"messages"`) {
		t.Errorf("body 应含 param=messages: %s", body)
	}
}

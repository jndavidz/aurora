package jwtutil

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func mkJWT(t *testing.T, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestExpNumericFuture(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	got, ok := Exp(mkJWT(t, map[string]any{"exp": exp}))
	if !ok || got.Unix() != exp {
		t.Fatalf("got=%v ok=%v want unix=%d", got, ok, exp)
	}
}

func TestExpNumericPast(t *testing.T) {
	exp := time.Now().Add(-time.Hour).Unix()
	got, ok := Exp(mkJWT(t, map[string]any{"exp": exp}))
	if !ok || got.Unix() != exp {
		t.Fatalf("got=%v ok=%v want unix=%d", got, ok, exp)
	}
}

func TestExpStringNumber(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	got, ok := Exp(mkJWT(t, map[string]any{"exp": strconv.FormatInt(exp, 10)}))
	if !ok || got.Unix() != exp {
		t.Fatalf("string exp: got=%v ok=%v want unix=%d", got, ok, exp)
	}
}

func TestExpMissing(t *testing.T) {
	if _, ok := Exp(mkJWT(t, map[string]any{"sub": "x"})); ok {
		t.Fatal("缺 exp 应 ok=false")
	}
}

func TestExpInvalidInputs(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "a.b.c", mkJWT(t, map[string]any{"exp": "NaN"})} {
		if _, ok := Exp(tok); ok {
			t.Fatalf("非法输入 %q 应 ok=false", tok)
		}
	}
}

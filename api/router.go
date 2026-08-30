package api

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"aurora/internal/bootstrap"
)

var (
	router   *gin.Engine
	initOnce sync.Once
	initErr  error
)

// Initialize 初始化 Serverless 适配入口的路由。
// 与 main.go 的 bootstrap.Init() 互斥:同一进程只能初始化一次,避免
// 双重初始化(双份账号池 / 双份后台协程 / 对上游的双倍登录请求)。
// 之前的 init() 实现会在任何 import 本包时静默执行完整 bootstrap,
// 一旦被 Serverless 入口之外的文件引用即构成双重初始化,故改为显式调用。
func Initialize() error {
	initOnce.Do(func() {
		app, err := bootstrap.Init()
		if err != nil {
			initErr = err
			return
		}
		router = app.Router
	})
	return initErr
}

// Listen 是 Serverless 平台的 http.HandlerFunc 适配入口。
func Listen(w http.ResponseWriter, r *http.Request) {
	if err := Initialize(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	router.ServeHTTP(w, r)
}

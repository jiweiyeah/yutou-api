package router

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// /v1/messages/count_tokens 与既有的 /v1/messages 同处一棵静态路由子树，
// 若注册冲突 Gin 会在建树时 panic。
func TestRelayRouterRegistersWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	require.NotPanics(t, func() {
		SetRelayRouter(engine)
	})
}

func TestCountTokensRouteIsRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetRelayRouter(engine)

	var registered []string
	for _, route := range engine.Routes() {
		registered = append(registered, route.Method+" "+route.Path)
	}
	assert.Contains(t, registered, http.MethodPost+" /v1/messages/count_tokens")
	assert.Contains(t, registered, http.MethodPost+" /v1/messages")
}

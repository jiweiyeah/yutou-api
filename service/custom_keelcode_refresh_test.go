package service

// ===== CUSTOM START: keelcode token 自动续期 =====

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body map[string]any) {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func decodeRequestBody(r *http.Request, out *map[string]any) error {
	return common.DecodeJson(io.LimitReader(r.Body, 1<<20), out)
}

// keelcodeStubServer 模拟 better-auth 的 device authorization 端点，
// 并记录每一步收到的凭据，用于断言旧 token 确实被当作身份使用。
type keelcodeStubServer struct {
	claimAuth   string
	approveAuth string
	approveBody map[string]any
	pending     atomic.Int32
}

func newKeelcodeStub(t *testing.T, stub *keelcodeStubServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"device_code": "device-code-1",
			"user_code":   "USER-CODE",
		})
	})
	mux.HandleFunc("/api/auth/device", func(w http.ResponseWriter, r *http.Request) {
		stub.claimAuth = r.Header.Get("Authorization")
		assert.Equal(t, "USER-CODE", r.URL.Query().Get("user_code"))
		writeJSON(t, w, http.StatusOK, map[string]any{"status": "pending"})
	})
	mux.HandleFunc("/api/auth/device/approve", func(w http.ResponseWriter, r *http.Request) {
		stub.approveAuth = r.Header.Get("Authorization")
		assert.NotEmpty(t, r.Header.Get("Origin"))
		require.NoError(t, decodeRequestBody(r, &stub.approveBody))
		writeJSON(t, w, http.StatusOK, map[string]any{"status": "approved"})
	})
	mux.HandleFunc("/api/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		if stub.pending.Add(-1) >= 0 {
			writeJSON(t, w, http.StatusBadRequest, map[string]any{
				"error":             "authorization_pending",
				"error_description": "waiting for approval",
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"access_token": "new-token",
			"expires_in":   604799,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestRefreshKeelcodeTokenUsesOldTokenAsCredential(t *testing.T) {
	stub := &keelcodeStubServer{}
	server := newKeelcodeStub(t, stub)

	token, err := refreshKeelcodeToken(
		context.Background(),
		server.Client(),
		server.URL,
		"https://keelcode.ai",
		"old-token",
	)
	require.NoError(t, err)
	assert.Equal(t, "new-token", token.AccessToken)
	assert.Equal(t, int64(604799), token.ExpiresIn)
	assert.Equal(t, "Bearer old-token", stub.claimAuth)
	assert.Equal(t, "Bearer old-token", stub.approveAuth)
	assert.Equal(t, "USER-CODE", stub.approveBody["userCode"])
}

// authorization_pending 是正常轮询状态，不能当成失败提前退出。
func TestRefreshKeelcodeTokenPollsThroughAuthorizationPending(t *testing.T) {
	stub := &keelcodeStubServer{}
	stub.pending.Store(1)
	server := newKeelcodeStub(t, stub)

	token, err := refreshKeelcodeToken(
		context.Background(),
		server.Client(),
		server.URL,
		"https://keelcode.ai",
		"old-token",
	)
	require.NoError(t, err)
	assert.Equal(t, "new-token", token.AccessToken)
}

// 旧 token 已失效时必须返回 errKeelcodeTokenExpired，调用方据此把这把 key
// 计入 expired_keys 而不是普通请求错误 —— 它需要人工回到浏览器流程重取。
func TestRefreshKeelcodeTokenReportsExpiredOldToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"device_code": "device-code-1",
			"user_code":   "USER-CODE",
		})
	})
	mux.HandleFunc("/api/auth/device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, err := refreshKeelcodeToken(
		context.Background(),
		server.Client(),
		server.URL,
		"https://keelcode.ai",
		"dead-token",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, errKeelcodeTokenExpired)
}

func TestKeelcodeChannelIDsParsing(t *testing.T) {
	t.Setenv("KEELCODE_CHANNEL_IDS", " 10824, 10825 ,10824,, abc, -3 ")
	assert.Equal(t, []int{10824, 10825}, keelcodeChannelIDs())
}

func TestKeelcodeLeadHoursFallsBackOnInvalidValue(t *testing.T) {
	t.Setenv("KEELCODE_TOKEN_REFRESH_LEAD_HOURS", "0")
	assert.Equal(t, keelcodeDefaultLeadHours, keelcodeLeadHours())

	t.Setenv("KEELCODE_TOKEN_REFRESH_LEAD_HOURS", "12")
	assert.Equal(t, 12, keelcodeLeadHours())
}

// ===== CUSTOM END =====

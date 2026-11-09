package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDahlKeyHealthIDIsStableAndOpaque(t *testing.T) {
	first := dahlKeyHealthID("dahl_test_key")
	second := dahlKeyHealthID("dahl_test_key")
	other := dahlKeyHealthID("dahl_other_key")

	assert.Equal(t, first, second)
	assert.NotEqual(t, first, other)
	assert.NotContains(t, first, "dahl_test_key")
	assert.Len(t, first, 16)
}

func TestDahlBalanceDue(t *testing.T) {
	now := int64(1_800_000_000)
	interval := 4 * time.Hour
	const threshold int64 = 500_000

	tests := []struct {
		name   string
		health model.ChannelKeyHealth
		want   bool
	}{
		{name: "never checked", health: model.ChannelKeyHealth{}, want: true},
		{name: "high balance fresh", health: model.ChannelKeyHealth{Balance: 100_000_000, CheckedAt: now - 3600}, want: false},
		{name: "high balance stale", health: model.ChannelKeyHealth{Balance: 100_000_000, CheckedAt: now - 5*3600}, want: true},
		{name: "near threshold fresh", health: model.ChannelKeyHealth{Balance: 900_000, CheckedAt: now - 3600}, want: true},
		{name: "zero balance fresh", health: model.ChannelKeyHealth{Balance: 0, CheckedAt: now - 3600}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dahlBalanceDue(tt.health, now, interval, threshold))
		})
	}
}

func TestDahlShouldProbe(t *testing.T) {
	now := int64(1_800_000_000)
	idle := 5 * 24 * time.Hour
	retry := 6 * time.Hour
	const threshold int64 = 500_000

	tests := []struct {
		name   string
		health model.ChannelKeyHealth
		want   bool
	}{
		{name: "unknown balance is not probeable", health: model.ChannelKeyHealth{}, want: false},
		{name: "below threshold", health: model.ChannelKeyHealth{CheckedAt: now, Balance: 400_000}, want: false},
		{name: "unknown usage is probeable", health: model.ChannelKeyHealth{CheckedAt: now, Balance: 100_000_000}, want: true},
		{name: "used four days ago", health: model.ChannelKeyHealth{CheckedAt: now, Balance: 100_000_000, LastUsed: now - 4*86400}, want: false},
		{name: "used six days ago", health: model.ChannelKeyHealth{CheckedAt: now, Balance: 100_000_000, LastUsed: now - 6*86400}, want: true},
		{name: "probed recently", health: model.ChannelKeyHealth{CheckedAt: now, Balance: 100_000_000, LastUsed: now - 6*86400, LastProbe: now - 3600}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, dahlShouldProbe(tt.health, now, idle, retry, threshold))
		})
	}
}

func TestParseDahlAvailableTokens(t *testing.T) {
	balance, err := parseDahlAvailableTokens([]byte(`{"available_tokens":123456}`))
	require.NoError(t, err)
	assert.Equal(t, int64(123456), balance)

	_, err = parseDahlAvailableTokens([]byte(`{}`))
	require.Error(t, err)

	_, err = parseDahlAvailableTokens([]byte(`not-json`))
	require.Error(t, err)
}

func TestCheckDahlKeyBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, dahlBalancePath, r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"available_tokens":987654321}`))
	}))
	defer server.Close()

	result := checkDahlKeyBalance(context.Background(), server.Client(), server.URL, dahlKeyEntry{key: "test-key"}, 2*time.Second)
	require.NoError(t, result.err)
	assert.Equal(t, int64(987654321), result.balance)
	assert.False(t, result.authFailed)

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	result = checkDahlKeyBalance(context.Background(), unauthorized.Client(), unauthorized.URL, dahlKeyEntry{key: "test-key"}, 2*time.Second)
	require.Error(t, result.err)
	assert.True(t, result.authFailed)

	exhausted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "exhausted", http.StatusPaymentRequired)
	}))
	defer exhausted.Close()
	result = checkDahlKeyBalance(context.Background(), exhausted.Client(), exhausted.URL, dahlKeyEntry{key: "test-key"}, 2*time.Second)
	require.Error(t, result.err)
	assert.True(t, errors.Is(result.err, errDahlBalanceExhausted))
}

func TestProbeDahlKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, dahlChatPath, r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		body, err := common.Marshal(map[string]any{
			"model":      "MiniMaxAI/MiniMax-M2.7",
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 16,
			"stream":     false,
		})
		require.NoError(t, err)
		assert.JSONEq(t, string(body), readBody(t, r))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	result := probeDahlKey(context.Background(), server.Client(), server.URL, "MiniMaxAI/MiniMax-M2.7", dahlKeyEntry{key: "test-key"}, 16, 2*time.Second)
	require.NoError(t, result.err)
	assert.True(t, result.ok)

	exhausted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "exhausted", http.StatusPaymentRequired)
	}))
	defer exhausted.Close()
	result = probeDahlKey(context.Background(), exhausted.Client(), exhausted.URL, "model", dahlKeyEntry{key: "test-key"}, 1, 2*time.Second)
	require.Error(t, result.err)
	assert.True(t, result.exhausted)
}

func TestDahlProbeModelUsesMinimaxAndMapping(t *testing.T) {
	models := "minimax/minimax-m2.7,deepseek/deepseek-v4-flash-0731"
	mapping := `{"minimax/minimax-m2.7":"MiniMaxAI/MiniMax-M2.7"}`
	channel := &model.Channel{
		Models:       models,
		ModelMapping: &mapping,
	}

	assert.Equal(t, "MiniMaxAI/MiniMax-M2.7", dahlProbeModel(channel, ""))
	assert.Equal(t, "explicit-model", dahlProbeModel(channel, "explicit-model"))

	channel.ModelMapping = nil
	assert.Equal(t, "minimax/minimax-m2.7", dahlProbeModel(channel, ""))
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	require.NoError(t, err)
	return string(data)
}

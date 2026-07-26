package advancedcustom

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdaptorUsesExactRouteAndQueryAuth(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/messages",
				UpstreamPath: "https://upstream.example/v1/chat/completions?existing=1",
				Converter:    relayconvert.ConverterClaudeMessagesToOpenAIChat,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeQuery,
					Name:  "api_key",
					Value: "{api_key}",
				},
			},
		},
	})
	info.RequestURLPath = "/v1/messages?client=1"

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)

	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "https", parsedURL.Scheme)
	assert.Equal(t, "upstream.example", parsedURL.Host)
	assert.Equal(t, "/v1/chat/completions", parsedURL.Path)
	assert.Equal(t, "1", parsedURL.Query().Get("existing"))
	assert.Equal(t, "sk-test", parsedURL.Query().Get("api_key"))
}

func TestAdaptorJoinsUpstreamPathWithChannelBaseURL(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/proxy/v1/chat/completions?existing=1",
				Converter:    relayconvert.ConverterNone,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeQuery,
					Name:  "api_key",
					Value: "{api_key}",
				},
			},
		},
	})
	info.ChannelBaseUrl = "https://gateway.example/base"

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)

	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "https", parsedURL.Scheme)
	assert.Equal(t, "gateway.example", parsedURL.Host)
	assert.Equal(t, "/base/proxy/v1/chat/completions", parsedURL.Path)
	assert.Equal(t, "1", parsedURL.Query().Get("existing"))
	assert.Equal(t, "sk-test", parsedURL.Query().Get("api_key"))
}

func TestAdaptorReturnsErrorWhenUpstreamPathNeedsMissingBaseURL(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/v1/chat/completions",
				Converter:    relayconvert.ConverterNone,
			},
		},
	})
	info.ChannelBaseUrl = ""

	_, err := adaptor.GetRequestURL(info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base URL is required")
}

func TestAdaptorSetupRequestHeaderUsesDefaultBearerAuth(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "https://upstream.example/v1/chat/completions",
				Converter:    relayconvert.ConverterNone,
			},
		},
	})
	c := advancedCustomGinContext("/v1/chat/completions")
	header := http.Header{}

	require.NoError(t, adaptor.SetupRequestHeader(c, &header, info))
	assert.Equal(t, "Bearer sk-test", header.Get("Authorization"))
}

func TestAdaptorSetupRequestHeaderUsesConfiguredHeaderAuth(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "https://upstream.example/v1/chat/completions",
				Converter:    relayconvert.ConverterNone,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeHeader,
					Name:  "x-api-key",
					Value: "{api_key}",
				},
			},
		},
	})
	c := advancedCustomGinContext("/v1/chat/completions")
	header := http.Header{}

	require.NoError(t, adaptor.SetupRequestHeader(c, &header, info))
	assert.Empty(t, header.Get("Authorization"))
	assert.Equal(t, "sk-test", header.Get("x-api-key"))
}

func TestAdaptorSetupRequestHeaderAddsClaudeDefaultHeaders(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/messages",
				UpstreamPath: "https://api.anthropic.com/v1/messages",
				Converter:    relayconvert.ConverterNone,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeHeader,
					Name:  "x-api-key",
					Value: "{api_key}",
				},
			},
		},
	})
	info.RelayFormat = types.RelayFormatClaude
	c := advancedCustomGinContext("/v1/messages")
	header := http.Header{}

	require.NoError(t, adaptor.SetupRequestHeader(c, &header, info))
	assert.Equal(t, "sk-test", header.Get("x-api-key"))
	assert.Equal(t, "2023-06-01", header.Get("anthropic-version"))
}

func TestAdaptorReturnsErrorWhenNoRouteMatchesPath(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/messages",
				UpstreamPath: "https://upstream.example/v1/chat/completions",
				Converter:    relayconvert.ConverterClaudeMessagesToOpenAIChat,
			},
		},
	})
	info.RequestURLPath = "/v1/chat/completions"

	_, err := adaptor.GetRequestURL(info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support request path")
}

func TestAdaptorReplacesModelPlaceholderInRouteURL(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent",
				Converter:    relayconvert.ConverterOpenAIChatToGeminiContent,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeQuery,
					Name:  "key",
					Value: "{api_key}",
				},
			},
		},
	})
	info.UpstreamModelName = "gemini-2.5-flash"

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)

	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "/v1beta/models/gemini-2.5-flash:generateContent", parsedURL.Path)
	assert.Equal(t, "sk-test", parsedURL.Query().Get("key"))
	assert.Empty(t, parsedURL.Query().Get("alt"))
}

func TestAdaptorSwitchesGeminiGenerateContentURLForStream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent?existing=1",
				Converter:    relayconvert.ConverterOpenAIChatToGeminiContent,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeQuery,
					Name:  "key",
					Value: "{api_key}",
				},
			},
		},
	})
	info.UpstreamModelName = "gemini-2.5-pro"
	info.IsStream = true

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)

	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "/v1beta/models/gemini-2.5-pro:streamGenerateContent", parsedURL.Path)
	assert.Equal(t, "sse", parsedURL.Query().Get("alt"))
	assert.Equal(t, "1", parsedURL.Query().Get("existing"))
	assert.Equal(t, "sk-test", parsedURL.Query().Get("key"))
}

func TestAdaptorMatchesGeminiIncomingPathTemplate(t *testing.T) {
	tests := []struct {
		name            string
		requestURLPath  string
		wantRequestPath string
	}{
		{
			name:            "generate content",
			requestURLPath:  "/v1beta/models/gemini-2.5-flash:generateContent",
			wantRequestPath: "/v1/chat/completions",
		},
		{
			name:            "stream generate content",
			requestURLPath:  "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
			wantRequestPath: "/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adaptor := &Adaptor{}
			info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
				Routes: []dto.AdvancedCustomRoute{
					{
						IncomingPath: "/v1beta/models/{model}:generateContent",
						UpstreamPath: "https://upstream.example/v1/chat/completions",
						Converter:    relayconvert.ConverterGeminiContentToOpenAIChat,
					},
				},
			})
			info.RequestURLPath = tt.requestURLPath

			requestURL, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)

			parsedURL, err := url.Parse(requestURL)
			require.NoError(t, err)
			assert.Equal(t, tt.wantRequestPath, parsedURL.Path)
		})
	}
}

func TestAdaptorBuildModelListRequestUsesConfiguredRouteAuth(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/models",
				UpstreamPath: "/provider/models",
				Converter:    relayconvert.ConverterNone,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeHeader,
					Name:  "x-api-key",
					Value: "token {api_key}",
				},
			},
		},
	})
	info.RequestURLPath = "/v1/models"

	requestURL, header, err := adaptor.BuildModelListRequest(info)
	require.NoError(t, err)

	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "fallback.example", parsedURL.Host)
	assert.Equal(t, "/provider/models", parsedURL.Path)
	assert.Equal(t, "token sk-test", header.Get("x-api-key"))
	assert.Empty(t, header.Get("Authorization"))
}

func TestAdaptorBuildModelListRequestUsesConfiguredQueryAuth(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/models",
				UpstreamPath: "https://upstream.example/v1/models?existing=1",
				Converter:    relayconvert.ConverterNone,
				Auth: &dto.AdvancedCustomRouteAuth{
					Type:  dto.AdvancedCustomAuthTypeQuery,
					Name:  "key",
					Value: "{api_key}",
				},
			},
		},
	})
	info.RequestURLPath = "/v1/models"

	requestURL, header, err := adaptor.BuildModelListRequest(info)
	require.NoError(t, err)

	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "upstream.example", parsedURL.Host)
	assert.Equal(t, "/v1/models", parsedURL.Path)
	assert.Equal(t, "1", parsedURL.Query().Get("existing"))
	assert.Equal(t, "sk-test", parsedURL.Query().Get("key"))
	assert.Empty(t, header.Get("Authorization"))
}

func TestAdaptorBuildModelListRequestDefaultAndNoAuth(t *testing.T) {
	tests := []struct {
		name              string
		auth              *dto.AdvancedCustomRouteAuth
		wantAuthorization string
	}{
		{
			name:              "default bearer",
			wantAuthorization: "Bearer sk-test",
		},
		{
			name: "no authentication",
			auth: &dto.AdvancedCustomRouteAuth{
				Type: dto.AdvancedCustomAuthTypeNone,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
				Routes: []dto.AdvancedCustomRoute{
					{
						IncomingPath: dto.AdvancedCustomModelListPath,
						UpstreamPath: "/provider/models",
						Auth:         tt.auth,
					},
				},
			})
			info.RequestURLPath = "/unrelated/path"

			requestURL, header, err := (&Adaptor{}).BuildModelListRequest(info)
			require.NoError(t, err)
			assert.Equal(t, "https://fallback.example/provider/models", requestURL)
			assert.Equal(t, tt.wantAuthorization, header.Get("Authorization"))
		})
	}
}

func TestAdaptorBuildModelListRequestDoesNotReuseRelayRoute(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/chat",
			},
			{
				IncomingPath: dto.AdvancedCustomModelListPath,
				UpstreamPath: "/provider/models",
			},
		},
	})

	chatURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://fallback.example/chat", chatURL)

	modelURL, header, err := adaptor.BuildModelListRequest(info)
	require.NoError(t, err)
	assert.Equal(t, "https://fallback.example/provider/models", modelURL)
	assert.Equal(t, "Bearer sk-test", header.Get("Authorization"))
}

func TestAdaptorBuildModelListRequestRequiresConfiguredRoute(t *testing.T) {
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/v1/chat/completions",
			},
		},
	})

	_, _, err := (&Adaptor{}).BuildModelListRequest(info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not configure a /v1/models route")
}

func TestAdaptorConvertsResponsesRequestToOpenAIChatUpstream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1/chat/completions",
				Converter:    relayconvert.ConverterOpenAIResponsesToOpenAIChat,
			},
		},
	})
	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	converted, err := adaptor.ConvertOpenAIResponsesRequest(c, info, dto.OpenAIResponsesRequest{
		Model:        "gpt-test",
		Instructions: mustAdvancedCustomRawMessage(t, "system rules"),
		Input:        mustAdvancedCustomRawMessage(t, "hello"),
	})
	require.NoError(t, err)

	chatReq, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	assert.Equal(t, "gpt-test", chatReq.Model)
	require.Len(t, chatReq.Messages, 2)
	assert.Equal(t, "system", chatReq.Messages[0].Role)
	assert.Equal(t, "system rules", chatReq.Messages[0].StringContent())
	assert.Equal(t, "user", chatReq.Messages[1].Role)
	assert.Equal(t, "hello", chatReq.Messages[1].StringContent())

	requestURL, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "/v1/chat/completions", parsedURL.Path)
}

func TestAdaptorLovableResponsesConverterHandlesToolFields(t *testing.T) {
	tests := []struct {
		name              string
		converter         string
		tools             any
		wantTools         bool
		wantToolChoice    bool
		wantParallelCalls bool
		wantReasoning     string
	}{
		{
			name:          "lovable omits tool fields without tools",
			converter:     ConverterOpenAIResponsesToOpenAIChatLovable,
			wantReasoning: "medium",
		},
		{
			name:      "lovable preserves tool fields with tools",
			converter: ConverterOpenAIResponsesToOpenAIChatLovable,
			tools: []map[string]any{
				{
					"type":       "function",
					"name":       "lookup",
					"parameters": map[string]any{"type": "object"},
				},
			},
			wantTools:         true,
			wantToolChoice:    true,
			wantParallelCalls: true,
			wantReasoning:     "none",
		},
		{
			name:              "generic converter remains unchanged without tools",
			converter:         relayconvert.ConverterOpenAIResponsesToOpenAIChat,
			wantToolChoice:    true,
			wantParallelCalls: true,
			wantReasoning:     "medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adaptor := &Adaptor{}
			info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
				Routes: []dto.AdvancedCustomRoute{
					{
						IncomingPath: "/v1/responses",
						UpstreamPath: "/v1/chat/completions",
						Converter:    tt.converter,
					},
				},
			})
			info.RelayMode = relayconstant.RelayModeResponses
			info.RequestURLPath = "/v1/responses"

			request := dto.OpenAIResponsesRequest{
				Model:             "gpt-test",
				Input:             mustAdvancedCustomRawMessage(t, "hello"),
				ToolChoice:        mustAdvancedCustomRawMessage(t, "auto"),
				ParallelToolCalls: mustAdvancedCustomRawMessage(t, true),
				Reasoning:         &dto.Reasoning{Effort: "medium"},
			}
			if tt.tools != nil {
				request.Tools = mustAdvancedCustomRawMessage(t, tt.tools)
			}

			converted, err := adaptor.ConvertOpenAIResponsesRequest(
				advancedCustomGinContext("/v1/responses"),
				info,
				request,
			)
			require.NoError(t, err)

			chatRequest, ok := converted.(*dto.GeneralOpenAIRequest)
			require.True(t, ok)
			if tt.wantTools {
				require.Len(t, chatRequest.Tools, 1)
				assert.Equal(t, "lookup", chatRequest.Tools[0].Function.Name)
			} else {
				assert.Empty(t, chatRequest.Tools)
			}
			if tt.wantToolChoice {
				assert.Equal(t, "auto", chatRequest.ToolChoice)
			} else {
				assert.Nil(t, chatRequest.ToolChoice)
			}
			if tt.wantParallelCalls {
				require.NotNil(t, chatRequest.ParallelTooCalls)
				assert.True(t, *chatRequest.ParallelTooCalls)
			} else {
				assert.Nil(t, chatRequest.ParallelTooCalls)
			}
			assert.Equal(t, tt.wantReasoning, chatRequest.ReasoningEffort)

			body, err := common.Marshal(chatRequest)
			require.NoError(t, err)
			var upstreamPayload map[string]any
			require.NoError(t, common.Unmarshal(body, &upstreamPayload))
			_, hasTools := upstreamPayload["tools"]
			_, hasToolChoice := upstreamPayload["tool_choice"]
			_, hasParallelCalls := upstreamPayload["parallel_tool_calls"]
			assert.Equal(t, tt.wantTools, hasTools)
			assert.Equal(t, tt.wantToolChoice, hasToolChoice)
			assert.Equal(t, tt.wantParallelCalls, hasParallelCalls)
		})
	}
}

func TestAdaptorLovableResponsesConverterNormalizesCustomToolGrammar(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1/chat/completions",
				Converter:    ConverterOpenAIResponsesToOpenAIChatLovable,
			},
		},
	})
	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"

	converted, err := adaptor.ConvertOpenAIResponsesRequest(
		advancedCustomGinContext("/v1/responses"),
		info,
		dto.OpenAIResponsesRequest{
			Model: "gpt-test",
			Input: mustAdvancedCustomRawMessage(t, []map[string]any{
				{
					"type": "additional_tools",
					"role": "developer",
					"tools": []map[string]any{
						{
							"type":        "custom",
							"name":        "apply_patch",
							"description": "Apply a patch",
							"format": map[string]any{
								"type":       "grammar",
								"syntax":     "lark",
								"definition": "start: PATCH",
							},
						},
						{
							"type":      "tool_search",
							"execution": "client",
						},
						{
							"type": "web_search",
						},
					},
				},
				{
					"type":    "message",
					"role":    "user",
					"content": []map[string]any{{"type": "input_text", "text": "hello"}},
				},
				{
					"type":    "custom_tool_call",
					"call_id": "call_apply_patch",
					"name":    "apply_patch",
					"input":   "*** Begin Patch",
				},
				{
					"type":    "custom_tool_call_output",
					"call_id": "call_apply_patch",
					"output":  "Done!",
				},
			}),
			ToolChoice:        mustAdvancedCustomRawMessage(t, "auto"),
			ParallelToolCalls: mustAdvancedCustomRawMessage(t, true),
			Reasoning:         &dto.Reasoning{Effort: "medium"},
		},
	)
	require.NoError(t, err)

	chatRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.Len(t, chatRequest.Tools, 1)
	assert.Equal(t, dto.CustomType, chatRequest.Tools[0].Type)
	assert.JSONEq(t, `{
		"name": "apply_patch",
		"description": "Apply a patch",
		"format": {
			"type": "grammar",
			"grammar": {
				"syntax": "lark",
				"definition": "start: PATCH"
			}
		}
	}`, string(chatRequest.Tools[0].Custom))
	assert.Equal(t, "auto", chatRequest.ToolChoice)
	require.NotNil(t, chatRequest.ParallelTooCalls)
	assert.True(t, *chatRequest.ParallelTooCalls)
	assert.Equal(t, "none", chatRequest.ReasoningEffort)
	require.Len(t, chatRequest.Messages, 3)
	assert.Equal(t, "user", chatRequest.Messages[0].Role)
	assert.Equal(t, "assistant", chatRequest.Messages[1].Role)
	toolCalls := chatRequest.Messages[1].ParseToolCalls()
	require.Len(t, toolCalls, 1)
	assert.Equal(t, dto.CustomType, toolCalls[0].Type)
	assert.Equal(t, "call_apply_patch", toolCalls[0].ID)
	assert.Equal(t, "tool", chatRequest.Messages[2].Role)
	assert.Equal(t, "call_apply_patch", chatRequest.Messages[2].ToolCallId)
	assert.Equal(t, "Done!", chatRequest.Messages[2].StringContent())
}

func TestAdaptorLovableConvertsCustomToolResponse(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1/chat/completions",
				Converter:    ConverterOpenAIResponsesToOpenAIChatLovable,
			},
		},
	})
	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "lovable-test")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl_upstream",
			"object":"chat.completion",
			"created":1710000000,
			"model":"gpt-test",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","tool_calls":[{
					"id":"call_exec",
					"type":"custom",
					"custom":{"name":"exec","input":"git rev-list --count HEAD"}
				}]},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}
		}`)),
	}

	usage, apiError := adaptor.DoResponse(c, response, info)
	require.Nil(t, apiError)
	parsedUsage, ok := usage.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 5, parsedUsage.TotalTokens)

	var payload map[string]any
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	outputs, ok := payload["output"].([]any)
	require.True(t, ok)
	require.Len(t, outputs, 1)
	output, ok := outputs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, lovableCustomToolCallType, output["type"])
	assert.Equal(t, "call_exec", output["call_id"])
	assert.Equal(t, "exec", output["name"])
	assert.Equal(t, "git rev-list --count HEAD", output["input"])
	assert.NotContains(t, output, "arguments")
}

func TestAdaptorLovableConvertsCustomToolStreamResponse(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1/chat/completions",
				Converter:    ConverterOpenAIResponsesToOpenAIChatLovable,
			},
		},
	})
	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"
	info.IsStream = true
	info.DisablePing = true

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "lovable-stream-test")
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_exec","type":"custom","custom":{"name":"exec"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"git rev-list"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":" --count HEAD"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1710000000,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, apiError := adaptor.DoResponse(c, response, info)
	require.Nil(t, apiError)
	parsedUsage, ok := usage.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 5, parsedUsage.TotalTokens)

	streamBody := recorder.Body.String()
	assert.Contains(t, streamBody, `event: response.custom_tool_call_input.delta`)
	assert.Contains(t, streamBody, `"type":"custom_tool_call"`)
	assert.Contains(t, streamBody, `"name":"exec"`)
	assert.Contains(t, streamBody, `"input":"git rev-list --count HEAD"`)
	assert.NotContains(t, streamBody, `event: response.function_call_arguments.delta`)
	assert.NotContains(t, streamBody, `event: response.function_call_arguments.done`)
}

func TestNormalizeLovableChatToolsTruncatesDescriptions(t *testing.T) {
	longDescription := strings.Repeat("界", lovableMaxToolDescriptionLength+1)
	custom, err := common.Marshal(map[string]any{
		"name":        "exec",
		"description": longDescription,
	})
	require.NoError(t, err)

	tools, err := normalizeLovableChatTools([]dto.ToolCallRequest{
		{
			Type:   dto.CustomType,
			Custom: custom,
		},
		{
			Type: "function",
			Function: dto.FunctionRequest{
				Name:        "wait",
				Description: longDescription,
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, tools, 2)

	var normalizedCustom map[string]any
	require.NoError(t, common.Unmarshal(tools[0].Custom, &normalizedCustom))
	assert.Len(t, []rune(normalizedCustom["description"].(string)), lovableMaxToolDescriptionLength)
	assert.Len(t, []rune(tools[1].Function.Description), lovableMaxToolDescriptionLength)
}

func TestAdaptorSelectsDuplicateResponsesRoutesByModel(t *testing.T) {
	config := &dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1/chat/completions",
				Converter:    relayconvert.ConverterOpenAIResponsesToOpenAIChat,
				Models:       []string{"gpt-test"},
			},
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1beta/models/{model}:generateContent",
				Converter:    relayconvert.ConverterOpenAIResponsesToGemini,
				Models:       []string{"gemini-test"},
			},
		},
	}

	chatAdaptor := &Adaptor{}
	chatInfo := advancedCustomRelayInfo(config)
	chatInfo.RelayFormat = types.RelayFormatOpenAIResponses
	chatInfo.RelayMode = relayconstant.RelayModeResponses
	chatInfo.RequestURLPath = "/v1/responses"
	chatInfo.OriginModelName = "gpt-test"
	chatInfo.UpstreamModelName = "gpt-test"
	chatConverted, err := chatAdaptor.ConvertOpenAIResponsesRequest(advancedCustomGinContext("/v1/responses"), chatInfo, dto.OpenAIResponsesRequest{
		Model: "gpt-test",
		Input: mustAdvancedCustomRawMessage(t, "hello"),
	})
	require.NoError(t, err)
	_, ok := chatConverted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)

	geminiAdaptor := &Adaptor{}
	geminiInfo := advancedCustomRelayInfo(config)
	geminiInfo.RelayFormat = types.RelayFormatOpenAIResponses
	geminiInfo.RelayMode = relayconstant.RelayModeResponses
	geminiInfo.RequestURLPath = "/v1/responses"
	geminiInfo.OriginModelName = "gemini-test"
	geminiInfo.UpstreamModelName = "gemini-test"
	geminiInfo.IsStream = true
	geminiConverted, err := geminiAdaptor.ConvertOpenAIResponsesRequest(advancedCustomGinContext("/v1/responses"), geminiInfo, dto.OpenAIResponsesRequest{
		Model: "gemini-test",
		Input: mustAdvancedCustomRawMessage(t, "hello"),
	})
	require.NoError(t, err)
	_, ok = geminiConverted.(*dto.GeminiChatRequest)
	require.True(t, ok)

	requestURL, err := geminiAdaptor.GetRequestURL(geminiInfo)
	require.NoError(t, err)
	parsedURL, err := url.Parse(requestURL)
	require.NoError(t, err)
	assert.Equal(t, "/v1beta/models/gemini-test:streamGenerateContent", parsedURL.Path)
	assert.Equal(t, "sse", parsedURL.Query().Get("alt"))
}

func TestAdaptorResponsesToGeminiUsesResponsesBridge(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1beta/models/{model}:generateContent",
				Converter:    relayconvert.ConverterOpenAIResponsesToGemini,
				Models:       []string{"gemini-test"},
			},
		},
	})
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"
	info.OriginModelName = "gemini-test"
	info.UpstreamModelName = "gemini-test"
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	payload := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "hello"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:     2,
			CandidatesTokenCount: 3,
			TotalTokenCount:      5,
		},
	}
	body, err := common.Marshal(payload)
	require.NoError(t, err)

	usage, newAPIError := adaptor.DoResponse(c, &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}, info)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)

	got := recorder.Body.String()
	assert.Contains(t, got, `"object":"response"`)
	assert.Contains(t, got, `"type":"output_text"`)
	assert.Contains(t, got, `"text":"hello"`)
	assert.NotContains(t, got, `"candidates"`)
}

func TestAdaptorResponsesToGeminiAddsThoughtSignatureForFunctionCallHistory(t *testing.T) {
	geminiSettings := model_setting.GetGeminiSettings()
	originalThoughtSignatureEnabled := geminiSettings.FunctionCallThoughtSignatureEnabled
	geminiSettings.FunctionCallThoughtSignatureEnabled = true
	t.Cleanup(func() {
		geminiSettings.FunctionCallThoughtSignatureEnabled = originalThoughtSignatureEnabled
	})

	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/responses",
				UpstreamPath: "/v1beta/models/{model}:generateContent",
				Converter:    relayconvert.ConverterOpenAIResponsesToGemini,
				Models:       []string{"gemini-test"},
			},
		},
	})
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"
	info.OriginModelName = "gemini-test"
	info.UpstreamModelName = "gemini-test"

	converted, err := adaptor.ConvertOpenAIResponsesRequest(advancedCustomGinContext("/v1/responses"), info, dto.OpenAIResponsesRequest{
		Model: "gemini-test",
		Input: mustAdvancedCustomRawMessage(t, []map[string]any{
			{
				"role":    "user",
				"content": "hi",
			},
			{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "glob",
				"arguments": map[string]any{"query": "*"},
			},
			{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output":  []map[string]any{{"path": "report.md"}},
			},
		}),
		Tools: mustAdvancedCustomRawMessage(t, []map[string]any{
			{"type": "function", "name": "glob", "parameters": map[string]any{"type": "object"}},
		}),
	})
	require.NoError(t, err)

	geminiReq, ok := converted.(*dto.GeminiChatRequest)
	require.True(t, ok)
	require.Len(t, geminiReq.Contents, 3)
	require.Len(t, geminiReq.Contents[1].Parts, 1)
	require.NotNil(t, geminiReq.Contents[1].Parts[0].FunctionCall)
	assert.NotEmpty(t, geminiReq.Contents[1].Parts[0].ThoughtSignature)
	require.Len(t, geminiReq.Contents[2].Parts, 1)
	require.NotNil(t, geminiReq.Contents[2].Parts[0].FunctionResponse)
	assert.Empty(t, geminiReq.Contents[2].Parts[0].ThoughtSignature)
}

func TestAdaptorConvertsOpenAIChatRequestToResponsesUpstream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/v1/responses",
				Converter:    relayconvert.ConverterOpenAIChatToOpenAIResponses,
			},
		},
	})
	c := advancedCustomGinContext("/v1/chat/completions")

	converted, err := adaptor.ConvertOpenAIRequest(c, info, &dto.GeneralOpenAIRequest{
		Model: "gpt-test",
		Messages: []dto.Message{
			{Role: "user", Content: "hello"},
		},
	})
	require.NoError(t, err)

	responsesReq, ok := converted.(*dto.OpenAIResponsesRequest)
	require.True(t, ok)
	assert.Equal(t, "gpt-test", responsesReq.Model)
	assert.NotEmpty(t, responsesReq.Input)
}

func TestAdaptorConvertsOpenAIChatRequestToClaudeUpstream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/v1/messages",
				Converter:    relayconvert.ConverterOpenAIChatToClaudeMessages,
			},
		},
	})
	c := advancedCustomGinContext("/v1/chat/completions")

	converted, err := adaptor.ConvertOpenAIRequest(c, info, &dto.GeneralOpenAIRequest{
		Model: "claude-test",
		Messages: []dto.Message{
			{Role: "user", Content: "hello"},
		},
	})
	require.NoError(t, err)

	claudeReq, ok := converted.(*dto.ClaudeRequest)
	require.True(t, ok)
	assert.Equal(t, "claude-test", claudeReq.Model)
	require.Len(t, claudeReq.Messages, 1)
	assert.Equal(t, "user", claudeReq.Messages[0].Role)
}

func TestAdaptorConvertsOpenAIChatRequestToGeminiUpstream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/chat/completions",
				UpstreamPath: "/v1beta/models/{model}:generateContent",
				Converter:    relayconvert.ConverterOpenAIChatToGeminiContent,
			},
		},
	})
	info.UpstreamModelName = "gemini-2.5-flash"
	c := advancedCustomGinContext("/v1/chat/completions")

	converted, err := adaptor.ConvertOpenAIRequest(c, info, &dto.GeneralOpenAIRequest{
		Model: "gemini-2.5-flash",
		Messages: []dto.Message{
			{Role: "user", Content: "hello"},
		},
	})
	require.NoError(t, err)

	geminiReq, ok := converted.(*dto.GeminiChatRequest)
	require.True(t, ok)
	require.Len(t, geminiReq.Contents, 1)
	assert.Equal(t, "user", geminiReq.Contents[0].Role)
}

func TestAdaptorConvertsClaudeRequestToOpenAIChatUpstream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1/messages",
				UpstreamPath: "/v1/chat/completions",
				Converter:    relayconvert.ConverterClaudeMessagesToOpenAIChat,
			},
		},
	})
	info.RelayFormat = types.RelayFormatClaude
	info.RequestURLPath = "/v1/messages"
	c := advancedCustomGinContext("/v1/messages")

	converted, err := adaptor.ConvertClaudeRequest(c, info, &dto.ClaudeRequest{
		Model: "gpt-test",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	})
	require.NoError(t, err)

	chatReq, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	assert.Equal(t, "gpt-test", chatReq.Model)
	require.Len(t, chatReq.Messages, 1)
	assert.Equal(t, "user", chatReq.Messages[0].Role)
}

func TestAdaptorConvertsGeminiRequestToOpenAIChatUpstream(t *testing.T) {
	adaptor := &Adaptor{}
	info := advancedCustomRelayInfo(&dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: "/v1beta/models/{model}:generateContent",
				UpstreamPath: "/v1/chat/completions",
				Converter:    relayconvert.ConverterGeminiContentToOpenAIChat,
			},
		},
	})
	info.RelayFormat = types.RelayFormatGemini
	info.RequestURLPath = "/v1beta/models/gemini-2.5-flash:generateContent"
	info.UpstreamModelName = "gpt-test"
	c := advancedCustomGinContext("/v1beta/models/gemini-2.5-flash:generateContent")

	converted, err := adaptor.ConvertGeminiRequest(c, info, &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{
			{
				Role: "user",
				Parts: []dto.GeminiPart{
					{Text: "hello"},
				},
			},
		},
	})
	require.NoError(t, err)

	chatReq, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	assert.Equal(t, "gpt-test", chatReq.Model)
	require.Len(t, chatReq.Messages, 1)
	assert.Equal(t, "user", chatReq.Messages[0].Role)
}

func advancedCustomRelayInfo(config *dto.AdvancedCustomConfig) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		RelayMode:       relayconstant.RelayModeChatCompletions,
		RequestURLPath:  "/v1/chat/completions",
		OriginModelName: "gpt-test",
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey:            "sk-test",
			ChannelBaseUrl:    "https://fallback.example",
			ChannelType:       constant.ChannelTypeAdvancedCustom,
			UpstreamModelName: "gpt-test",
			ChannelOtherSettings: dto.ChannelOtherSettings{
				AdvancedCustom: config,
			},
		},
	}
}

func advancedCustomGinContext(path string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func mustAdvancedCustomRawMessage(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := common.Marshal(value)
	require.NoError(t, err)
	return raw
}

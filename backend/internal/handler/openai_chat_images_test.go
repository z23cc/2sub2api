//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	chatImageModel   = "gpt-image-1"
	chatImageMessage = `"messages":[{"role":"user","content":"Draw a cat"}]`
	chatImageRequest = `{"model":"gpt-image-1",` + chatImageMessage + `}`
	chatImageResult  = `{"created":1,"data":[{"b64_json":"aGk="}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}`
	chatImageContent = "![image](data:image/png;base64,aGk=)"
	chatImageError   = `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid image size","param":"size"}}`
	chatImageOSSURL  = "https://storage.example.com/cat.png"
)

type chatImageUpstreamCall struct {
	path string
	body string
}

type chatImageUpstream struct {
	service.HTTPUpstream
	status         int
	body           string
	wait           <-chan struct{}
	calls          []chatImageUpstreamCall
	beforeResponse func(*http.Request) error
}

func (u *chatImageUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.calls = append(u.calls, chatImageUpstreamCall{path: req.URL.Path, body: string(body)})
	if u.wait != nil {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-u.wait:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-timer.C:
			return nil, errors.New("waiting for SSE keepalive timed out")
		}
	}
	if u.beforeResponse != nil {
		if err := u.beforeResponse(req); err != nil {
			return nil, err
		}
	}
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_chat_image_test"}},
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func newChatImageHandler(t *testing.T, cfg *config.Config, upstream service.HTTPUpstream, cache service.BillingCache, logs service.UsageLogRepository, billing service.UsageBillingRepository) (*OpenAIGatewayHandler, *service.APIKey) {
	t.Helper()
	return newChatImageHandlerWithGatewaySettings(t, cfg, upstream, cache, logs, billing, nil, nil)
}

func newChatImageHandlerWithGatewaySettings(t *testing.T, cfg *config.Config, upstream service.HTTPUpstream, cache service.BillingCache, logs service.UsageLogRepository, billing service.UsageBillingRepository, gatewayCache service.GatewayCache, settings *service.SettingService) (*OpenAIGatewayHandler, *service.APIKey) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	group := &service.Group{
		ID: 3130, Hydrated: true, Status: service.StatusActive, Platform: service.PlatformOpenAI,
		AllowImageGeneration: true, RateMultiplier: 1,
	}
	repo := openAIImagesFailoverAccountRepo{accounts: []service.Account{{
		ID: 1, Name: "image-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{group.ID},
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://images.example.com"},
	}}}
	billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewOpenAIGatewayService(
		repo, nil, logs, billing, nil, nil, nil, gatewayCache, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, settings, nil,
	)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	apiKey := &service.APIKey{ID: 99, UserID: 100, GroupID: &group.ID, Group: group, User: &service.User{ID: 100, Balance: 100}}
	return h, apiKey
}

func newChatImageContext(writer http.ResponseWriter, apiKey *service.APIKey, path, body string) *gin.Context {
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	setChatImageAuth(c, apiKey)
	return c
}

func setChatImageAuth(c *gin.Context, apiKey *service.APIKey) {
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})
}

func testChatImageCompletion(t *testing.T, rec *httptest.ResponseRecorder, content string) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	var response apicompat.ChatCompletionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.True(t, strings.HasPrefix(response.ID, "chatcmpl-"), response.ID)
	require.Positive(t, response.Created)
	require.Equal(t, "chat.completion", response.Object)
	require.Equal(t, chatImageModel, response.Model)
	require.Len(t, response.Choices, 1)
	choice := response.Choices[0]
	require.Zero(t, choice.Index)
	require.Equal(t, "assistant", choice.Message.Role)
	var gotContent string
	require.NoError(t, json.Unmarshal(choice.Message.Content, &gotContent))
	require.Equal(t, content, gotContent)
	require.Equal(t, "stop", choice.FinishReason)
	require.Equal(t, &apicompat.ChatUsage{PromptTokens: 5, CompletionTokens: 10, TotalTokens: 15}, response.Usage)
}

func TestOpenAIChatImages_NonStreaming(t *testing.T) {
	for _, tt := range []struct {
		name     string
		request  string
		path     string
		wantBody string
	}{
		{
			name: "generation", request: chatImageRequest, path: "/v1/images/generations",
			wantBody: `{"model":"gpt-image-1","prompt":"Draw a cat","stream":false}`,
		},
		{
			name: "edit", path: "/v1/images/edits",
			request:  `{"model":"gpt-image-1","messages":[{"role":"user","content":[{"type":"text","text":"Add stripes"},{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`,
			wantBody: `{"model":"gpt-image-1","prompt":"Add stripes","stream":false,"images":[{"image_url":"https://example.com/cat.png"}]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
			h, apiKey := newChatImageHandler(t, &config.Config{RunMode: config.RunModeSimple}, upstream, nil, nil, nil)
			rec := httptest.NewRecorder()
			c := newChatImageContext(rec, apiKey, "/v1/chat/completions", tt.request)
			h.ChatCompletions(c)

			testChatImageCompletion(t, rec, chatImageContent)
			require.Equal(t, int64(1), c.Request.Context().Value(ctxkey.AccountID))
			require.Equal(t, service.PlatformOpenAI, c.Request.Context().Value(ctxkey.Platform))
			require.Len(t, upstream.calls, 1)
			require.Equal(t, tt.path, upstream.calls[0].path)
			require.JSONEq(t, tt.wantBody, upstream.calls[0].body)
		})
	}
}

type chatImageKeepaliveRecorder struct {
	*httptest.ResponseRecorder
	frame  string
	after  <-chan struct{}
	seen   chan struct{}
	status int
}

func (r *chatImageKeepaliveRecorder) Write(p []byte) (int, error) {
	observe := string(p) == r.frame
	if observe && r.after != nil {
		select {
		case <-r.after:
		default:
			observe = false
		}
	}
	n, err := r.ResponseRecorder.Write(p)
	if err == nil && observe {
		select {
		case <-r.seen:
		default:
			r.status = r.Code
			close(r.seen)
		}
	}
	return n, err
}

func TestOpenAIChatImages_Streaming(t *testing.T) {
	for _, tt := range []struct {
		name      string
		usage     bool
		keepalive bool
	}{
		{name: "without usage"},
		{name: "with usage and keepalive", usage: true, keepalive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
			rec := httptest.NewRecorder()
			var writer http.ResponseWriter = rec
			if tt.keepalive {
				cfg.Gateway.ImageStreamKeepaliveInterval = 1
				cfg.Gateway.ImageNonstreamKeepaliveInterval = 1
				seen := make(chan struct{})
				writer = &chatImageKeepaliveRecorder{ResponseRecorder: rec, frame: ": keepalive\n\n", seen: seen}
				upstream.wait = seen
			}
			h, apiKey := newChatImageHandler(t, cfg, upstream, nil, nil, nil)
			body := `{"model":"gpt-image-1","stream":true,` + chatImageMessage
			if tt.usage {
				body += `,"stream_options":{"include_usage":true}`
			}
			h.ChatCompletions(newChatImageContext(writer, apiKey, "/v1/chat/completions", body+"}"))

			frames := testChatImageSSEFrames(t, rec)
			if tt.keepalive {
				require.GreaterOrEqual(t, len(frames), 6, rec.Body.String())
				require.Equal(t, ": keepalive", frames[1], rec.Body.String())
				// All comments, including any extra ticks under a slow scheduler, must precede content.
				firstContent := 1
				for firstContent < len(frames) && frames[firstContent] == ": keepalive" {
					firstContent++
				}
				frames = append(frames[:1], frames[firstContent:]...)
			}
			wantFrameCount := 4
			if tt.usage {
				wantFrameCount++
			}
			require.Len(t, frames, wantFrameCount, rec.Body.String())
			require.Equal(t, "data: [DONE]", frames[len(frames)-1])
			require.Len(t, upstream.calls, 1)
			require.Equal(t, "/v1/images/generations", upstream.calls[0].path)
			require.JSONEq(t, `{"model":"gpt-image-1","prompt":"Draw a cat","stream":false}`, upstream.calls[0].body)

			chunks := make([]apicompat.ChatCompletionsChunk, wantFrameCount-1)
			for i := range chunks {
				require.True(t, strings.HasPrefix(frames[i], "data: "), frames[i])
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[i], "data: ")), &chunks[i]))
				require.Equal(t, "chat.completion.chunk", chunks[i].Object)
				require.Equal(t, chatImageModel, chunks[i].Model)
				require.Equal(t, chunks[0].ID, chunks[i].ID)
				require.Equal(t, chunks[0].Created, chunks[i].Created)
			}
			require.True(t, strings.HasPrefix(chunks[0].ID, "chatcmpl-"), chunks[0].ID)
			require.Positive(t, chunks[0].Created)
			stop, content := "stop", chatImageContent
			for i, choice := range []apicompat.ChatChunkChoice{
				{Delta: apicompat.ChatDelta{Role: "assistant"}},
				{Delta: apicompat.ChatDelta{Content: &content}},
				{FinishReason: &stop},
			} {
				require.Equal(t, []apicompat.ChatChunkChoice{choice}, chunks[i].Choices)
				require.Nil(t, chunks[i].Usage)
			}
			if tt.usage {
				require.Equal(t, []apicompat.ChatChunkChoice{}, chunks[3].Choices)
				require.Equal(t, &apicompat.ChatUsage{PromptTokens: 5, CompletionTokens: 10, TotalTokens: 15}, chunks[3].Usage)
			}
		})
	}
}

func testChatImageSSEFrames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.True(t, rec.Flushed)
	wire := rec.Body.String()
	require.True(t, strings.HasSuffix(wire, "\n\n"), wire)
	return strings.Split(strings.TrimSuffix(wire, "\n\n"), "\n\n")
}

func TestOpenAIChatImages_Errors(t *testing.T) {
	for _, tt := range []struct {
		name        string
		request     string
		platform    string
		allowImages bool
		stream      bool
		wantStatus  int
		wantCalls   int
		wantError   string
	}{
		{
			name: "upstream bad request", request: chatImageRequest,
			platform: service.PlatformOpenAI, allowImages: true, wantStatus: http.StatusBadRequest, wantCalls: 1, wantError: chatImageError,
		},
		{
			name:     "upstream bad request after streaming starts",
			request:  `{"model":"gpt-image-1","stream":true,` + chatImageMessage + `}`,
			platform: service.PlatformOpenAI, allowImages: true, stream: true, wantStatus: http.StatusOK, wantCalls: 1,
			wantError: `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid image size"}}`,
		},
		{
			name: "image generation is disabled", request: chatImageRequest,
			platform: service.PlatformOpenAI, wantStatus: http.StatusForbidden,
			wantError: `{"error":{"type":"permission_error","message":"Image generation is not enabled for this group"}}`,
		},
		{
			name:     "empty prompt",
			request:  `{"model":"gpt-image-1","messages":[{"role":"user","content":" "}]}`,
			platform: service.PlatformOpenAI, allowImages: true, wantStatus: http.StatusBadRequest,
			wantError: `{"error":{"type":"invalid_request_error","message":"the last user message must contain a text prompt"}}`,
		},
		{
			name:     "image part is missing URL",
			request:  `{"model":"gpt-image-1","messages":[{"role":"user","content":[{"type":"text","text":"Add stripes"},{"type":"image_url","image_url":{}}]}]}`,
			platform: service.PlatformOpenAI, allowImages: true, wantStatus: http.StatusBadRequest,
			wantError: `{"error":{"type":"invalid_request_error","message":"image_url content parts must include a non-empty url"}}`,
		},
		{
			name: "Grok keeps the original rejection", request: chatImageRequest,
			platform: service.PlatformGrok, allowImages: true, wantStatus: http.StatusBadRequest,
			wantError: `{"error":{"type":"invalid_request_error","message":"This model is not supported on the Chat Completions endpoint"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &chatImageUpstream{status: http.StatusBadRequest, body: chatImageError}
			logs := &chatImageUsageLogs{}
			h, apiKey := newChatImageHandler(t, &config.Config{RunMode: config.RunModeSimple}, upstream, nil, logs, nil)
			apiKey.Group.Platform = tt.platform
			apiKey.Group.AllowImageGeneration = tt.allowImages
			rec := httptest.NewRecorder()
			c := newChatImageContext(rec, apiKey, "/v1/chat/completions", tt.request)
			h.ChatCompletions(c)

			require.Len(t, upstream.calls, tt.wantCalls)
			require.Empty(t, logs.rows)
			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			if tt.stream {
				frames := testChatImageSSEFrames(t, rec)
				require.Len(t, frames, 3, rec.Body.String())
				require.True(t, strings.HasPrefix(frames[0], "data: "), frames[0])
				var first apicompat.ChatCompletionsChunk
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[0], "data: ")), &first))
				require.Equal(t, []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Role: "assistant"}}}, first.Choices)
				const errorPrefix = "event: error\ndata: "
				require.True(t, strings.HasPrefix(frames[1], errorPrefix), frames[1])
				require.JSONEq(t, tt.wantError, strings.TrimPrefix(frames[1], errorPrefix))
				require.Equal(t, "data: [DONE]", frames[2])
				marks := service.GetOpsStreamErrors(c)
				require.Len(t, marks, 1)
				require.True(t, marks[0].CountTowardsSLA)
				require.False(t, marks[0].NonStream)
				require.Equal(t, http.StatusBadRequest, marks[0].IntendedStatus)
			} else {
				require.JSONEq(t, tt.wantError, rec.Body.String())
			}
		})
	}
}

func TestOpenAIChatImages_RejectsRegionalFunds(t *testing.T) {
	for _, source := range []string{"config file", "gateway runtime"} {
		t.Run(source, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
			logs := &chatImageUsageLogs{}
			h, apiKey := newChatImageHandler(t, cfg, upstream, nil, logs, nil)
			if source == "config file" {
				cfg.Runtime.RegionalFundsFile = "x"
			} else {
				service.WithRegionalMetering(&service.RegionalMetering{})(h.gatewayService)
			}
			rec := httptest.NewRecorder()
			h.ChatCompletions(newChatImageContext(rec, apiKey, "/v1/chat/completions", chatImageRequest))

			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.JSONEq(t, `{"error":{"type":"regional_cost_bound_unavailable","message":"This protocol has no certified regional cost bound"}}`, rec.Body.String())
			require.Empty(t, upstream.calls)
			require.Empty(t, logs.rows)
		})
	}
}

func TestOpenAIChatImages_RejectsBlockedCyberSession(t *testing.T) {
	cache := testutil.NewRedisGatewayCache(t)
	settings := service.NewSettingService(&contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyCyberSessionBlockEnabled:          "true",
		service.SettingKeyCyberSessionIdentityStrictEnabled: "false",
	}}, nil)
	upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
	logs := &chatImageUsageLogs{}
	h, apiKey := newChatImageHandlerWithGatewaySettings(t, &config.Config{RunMode: config.RunModeSimple},
		upstream, nil, logs, nil, cache, settings)
	rec := httptest.NewRecorder()
	c := newChatImageContext(rec, apiKey, "/v1/chat/completions", chatImageRequest)
	c.Request.Header.Set("session_id", "blocked-image-session")
	blockKey := service.CyberSessionExplicitBlockKey(apiKey.ID, c, []byte(chatImageRequest))
	require.NotEmpty(t, blockKey)
	store, ok := cache.(service.CyberSessionBlockStore)
	require.True(t, ok)
	require.NoError(t, store.SetCyberSessionBlocked(t.Context(), "", []string{blockKey}, time.Minute))

	h.ChatCompletions(c)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "permission_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, "session_blocked_by_cyber_policy", gjson.Get(rec.Body.String(), "error.code").String())
	require.Empty(t, upstream.calls)
	require.Empty(t, logs.rows)
}

func TestOpenAIChatImages_LateNonStreamingErrorCountsTowardsSLA(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 4)
	rec := httptest.NewRecorder()
	writer := &chatImageKeepaliveRecorder{ResponseRecorder: rec, frame: " \n", seen: make(chan struct{})}
	upstream := &chatImageUpstream{status: http.StatusBadRequest, body: chatImageError, wait: writer.seen}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.ImageNonstreamKeepaliveInterval = 1
	h, apiKey := newChatImageHandler(t, cfg, upstream, nil, nil, nil)
	repo := &ingressRejectOpsRepo{}
	ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	var marks []service.OpsStreamError
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		setChatImageAuth(c, apiKey)
		h.ChatCompletions(c)
		marks = service.GetOpsStreamErrors(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatImageRequest))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(writer, req)

	require.Len(t, upstream.calls, 1)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, rec.Flushed)
	require.True(t, strings.HasPrefix(rec.Body.String(), writer.frame), rec.Body.String())
	require.JSONEq(t, chatImageError, strings.TrimSpace(rec.Body.String()))
	require.Len(t, marks, 1)
	require.True(t, marks[0].CountTowardsSLA)
	require.True(t, marks[0].NonStream)
	require.Equal(t, http.StatusBadRequest, marks[0].IntendedStatus)
	require.Equal(t, "invalid_value", marks[0].Code)
	require.Equal(t, int64(1), OpsErrorLogQueueLength())
	flushOpsErrorLogBatch([]opsErrorLogJob{<-opsErrorLogQueue})
	require.Len(t, repo.entries, 1)
	entry := repo.entries[0]
	require.Equal(t, http.StatusBadRequest, entry.StatusCode)
	require.False(t, entry.Stream)
	require.Equal(t, "Invalid image size", entry.ErrorMessage)
}

func TestOpenAIChatImages_PublicModel(t *testing.T) {
	const publicModel = "public-image"
	for _, tt := range []struct {
		name   string
		stream bool
	}{
		{name: "non-streaming"},
		{name: "streaming with usage", stream: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
			h, apiKey := newChatImageHandler(t, &config.Config{RunMode: config.RunModeSimple}, upstream, nil, nil, nil)
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"gpt-image-1","stream":%t,"stream_options":{"include_usage":true},`+chatImageMessage+`}`, tt.stream)
			c := newChatImageContext(rec, apiKey, "/v1/chat/completions", body)
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, publicModel))
			h.ChatCompletions(c)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			if tt.stream {
				frames := testChatImageSSEFrames(t, rec)
				require.Len(t, frames, 5)
				require.Equal(t, "data: [DONE]", frames[4])
				for _, frame := range frames[:4] {
					require.True(t, strings.HasPrefix(frame, "data: "), frame)
					require.Equal(t, publicModel, gjson.Get(strings.TrimPrefix(frame, "data: "), "model").String())
				}
			} else {
				require.Equal(t, publicModel, gjson.Get(rec.Body.String(), "model").String())
			}
			require.Equal(t, publicModel, c.GetString(opsModelKey))
			require.Len(t, upstream.calls, 1)
			require.Equal(t, chatImageModel, gjson.Get(upstream.calls[0].body, "model").String())
		})
	}
}

type chatImageStorage struct {
	key         string
	contentType string
	data        []byte
	calls       int
	err         error
	entered     chan struct{}
	release     <-chan struct{}
}

func (s *chatImageStorage) Save(ctx context.Context, key, contentType string, data []byte) (string, error) {
	s.key, s.contentType, s.data = key, contentType, data
	s.calls++
	if s.entered != nil {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return chatImageOSSURL, s.err
}

func TestOpenAIChatImages_ObjectStorage(t *testing.T) {
	const resultWithBothFormats = `{"created":1,"data":[{"b64_json":"aGk=","url":"https://upstream.example.com/cat.png"}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}`
	for _, tt := range []struct {
		name    string
		enabled bool
		saveErr error
		content string
	}{
		{name: "uploaded", enabled: true, content: "![image](" + chatImageOSSURL + ")"},
		{name: "disabled", content: chatImageContent},
		{name: "failed upload", enabled: true, saveErr: errors.New("test storage error"), content: chatImageContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &chatImageUpstream{status: http.StatusOK, body: resultWithBothFormats}
			h, apiKey := newChatImageHandler(t, &config.Config{RunMode: config.RunModeSimple}, upstream, nil, nil, nil)
			storage := &chatImageStorage{err: tt.saveErr}
			h.imageStorageResolver = func() (*service.ImageResultUploader, bool) {
				if !tt.enabled {
					return nil, false
				}
				return service.NewImageResultUploader(storage, "images/", 1024, nil), true
			}
			rec := httptest.NewRecorder()
			h.ChatCompletions(newChatImageContext(rec, apiKey, "/v1/chat/completions", chatImageRequest))

			testChatImageCompletion(t, rec, tt.content)
			if tt.enabled {
				require.Equal(t, 1, storage.calls)
				require.Regexp(t, "^images/chatimg_[a-f0-9]{32}-0\\.png$", storage.key)
				require.Equal(t, "image/png", storage.contentType)
				require.Equal(t, []byte("hi"), storage.data)
			} else {
				require.Zero(t, storage.calls)
			}
		})
	}
}

func TestOpenAIChatImages_KeepaliveDuringObjectStorageUpload(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stream bool
		frame  string
	}{
		{name: "streaming", stream: true, frame: ": keepalive\n\n"},
		{name: "non-streaming", frame: " \n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Gateway.ImageStreamKeepaliveInterval = 1
			cfg.Gateway.ImageNonstreamKeepaliveInterval = 1
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
			h, apiKey := newChatImageHandler(t, cfg, upstream, nil, nil, nil)
			release := make(chan struct{})
			storage := &chatImageStorage{entered: make(chan struct{}), release: release}
			h.imageStorageResolver = func() (*service.ImageResultUploader, bool) {
				return service.NewImageResultUploader(storage, "images/", 1024, nil), true
			}

			rec := httptest.NewRecorder()
			writer := &chatImageKeepaliveRecorder{
				ResponseRecorder: rec, frame: tt.frame, after: storage.entered, seen: make(chan struct{}),
			}
			body := fmt.Sprintf(`{"model":"gpt-image-1","stream":%t,`+chatImageMessage+`}`, tt.stream)
			c := newChatImageContext(writer, apiKey, "/v1/chat/completions", body)
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			c.Request = c.Request.WithContext(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.ChatCompletions(c)
			}()
			defer func() {
				cancel()
				<-done
			}()

			select {
			case <-storage.entered:
			case <-done:
				t.Fatal("handler returned before calling storage Save")
			case <-ctx.Done():
				t.Fatal("timed out waiting for storage Save")
			}
			select {
			case <-writer.seen:
				require.Equal(t, http.StatusOK, writer.status)
			case <-ctx.Done():
				t.Fatal("no keepalive was written after storage Save began")
			}
			close(release)
			<-done

			require.Equal(t, 1, storage.calls)
			content := "![image](" + chatImageOSSURL + ")"
			if !tt.stream {
				require.True(t, strings.HasPrefix(rec.Body.String(), tt.frame))
				require.True(t, rec.Flushed)
				testChatImageCompletion(t, rec, content)
				return
			}

			frames := testChatImageSSEFrames(t, rec)
			require.GreaterOrEqual(t, len(frames), 5, rec.Body.String())
			require.Equal(t, "data: [DONE]", frames[len(frames)-1])
			for _, frame := range frames[1 : len(frames)-3] {
				require.Equal(t, ": keepalive", frame)
			}
			stop := "stop"
			chunkFrames := []string{frames[0], frames[len(frames)-3], frames[len(frames)-2]}
			for i, want := range []apicompat.ChatChunkChoice{
				{Delta: apicompat.ChatDelta{Role: "assistant"}},
				{Delta: apicompat.ChatDelta{Content: &content}},
				{FinishReason: &stop},
			} {
				require.True(t, strings.HasPrefix(chunkFrames[i], "data: "), chunkFrames[i])
				var chunk apicompat.ChatCompletionsChunk
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(chunkFrames[i], "data: ")), &chunk))
				require.Equal(t, "chat.completion.chunk", chunk.Object)
				require.Equal(t, []apicompat.ChatChunkChoice{want}, chunk.Choices)
			}
		})
	}
}

type chatImageUsageLogs struct {
	service.UsageLogRepository
	rows []service.UsageLog
}

func (r *chatImageUsageLogs) Create(_ context.Context, row *service.UsageLog) (bool, error) {
	// Match the persistent repository's normalization at insertion.
	row.SyncRequestTypeAndLegacyFields()
	r.rows = append(r.rows, *row)
	return true, nil
}

func TestOpenAIChatImages_EmptyBilledResult(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result string
		stream bool
	}{
		{name: "empty data array", result: `{"created":1,"data":[]}`},
		{name: "streaming without a usable image", result: `{"created":1,"data":[{}]}`, stream: true},
		{name: "non-JSON upstream success", result: "not JSON"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pricePerImage := 0.125
			upstream := &chatImageUpstream{status: http.StatusOK, body: tt.result}
			logs := &chatImageUsageLogs{}
			billing := &compatStreamUsageBillingRepo{}
			h, apiKey := newChatImageHandler(t, &config.Config{RunMode: config.RunModeStandard},
				upstream, chatImageBillingCache{newHandlerInflightCache(100)}, logs, billing)
			apiKey.Group.ImageCountBilling = true
			apiKey.Group.ImagePrice1K = &pricePerImage
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"gpt-image-1","size":"1024x1024","stream":%t,`+chatImageMessage+`}`, tt.stream)
			c := newChatImageContext(rec, apiKey, "/v1/chat/completions", body)
			h.ChatCompletions(c)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			if tt.stream {
				frames := testChatImageSSEFrames(t, rec)
				require.Len(t, frames, 4, rec.Body.String())
				require.Equal(t, "data: [DONE]", frames[3])
				stop, content := "stop", chatImagesEmptyResultMessage
				for i, want := range []apicompat.ChatChunkChoice{
					{Delta: apicompat.ChatDelta{Role: "assistant"}},
					{Delta: apicompat.ChatDelta{Content: &content}},
					{FinishReason: &stop},
				} {
					require.True(t, strings.HasPrefix(frames[i], "data: "), frames[i])
					var chunk apicompat.ChatCompletionsChunk
					require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[i], "data: ")), &chunk))
					require.Equal(t, []apicompat.ChatChunkChoice{want}, chunk.Choices)
				}
			} else {
				require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
				result := gjson.Parse(rec.Body.String())
				require.Equal(t, "chat.completion", result.Get("object").String())
				require.Equal(t, chatImagesEmptyResultMessage, result.Get("choices.0.message.content").String())
				require.Equal(t, "stop", result.Get("choices.0.finish_reason").String())
			}
			require.Len(t, upstream.calls, 1)
			require.Len(t, logs.rows, 1)
			require.Equal(t, 1, logs.rows[0].ImageCount)
			require.Equal(t, pricePerImage, logs.rows[0].ActualCost)
			require.Len(t, billing.commands, 1)
			require.Equal(t, pricePerImage, billing.commands[0].BalanceCost)
			marks := service.GetOpsStreamErrors(c)
			require.Len(t, marks, 1)
			require.Equal(t, "image_generation_empty", marks[0].Code)
			require.Equal(t, http.StatusBadGateway, marks[0].IntendedStatus)
			require.True(t, marks[0].CountTowardsSLA)
			require.Equal(t, !tt.stream, marks[0].NonStream)
		})
	}
}

const (
	chatImageBillingDirectBody = `{"model":"gpt-image-1","n":2,"prompt":"Draw a cat","size":"1024x1024","stream":false}`
	chatImageBillingChatBody   = `{"model":"gpt-image-1","n":2,"size":"1024x1024","stream":%t,` + chatImageMessage + `}`
	chatImageBillingResult     = `{"created":1,"data":[{"b64_json":"aGk="},{"b64_json":"eWVz"}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}`
)

func TestOpenAIChatImages_BillingMatchesDirectImages(t *testing.T) {
	upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageBillingResult}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	logs := &chatImageUsageLogs{}
	billing := &compatStreamUsageBillingRepo{}
	h, apiKey := newChatImageHandler(t, cfg, upstream, chatImageBillingCache{newHandlerInflightCache(100)}, logs, billing)
	apiKey.Group.ImageCountBilling = true
	pricePerImage := 0.125
	apiKey.Group.ImagePrice1K = &pricePerImage

	for i, tt := range []struct {
		path        string
		body        string
		wantInbound string
		stream      bool
		serve       gin.HandlerFunc
	}{
		{"/v1/images/generations", chatImageBillingDirectBody, "/v1/images/generations", false, h.Images},
		{"/v1/chat/completions", fmt.Sprintf(chatImageBillingChatBody, false), "/v1/chat/completions", false, h.ChatCompletions},
		{"/chat/completions", fmt.Sprintf(chatImageBillingChatBody, true), "/chat/completions", true, h.ChatCompletions},
	} {
		rec := httptest.NewRecorder()
		c := newChatImageContext(rec, apiKey, tt.path, tt.body)
		c.Request.Header.Set("User-Agent", "chat-image-regression")
		c.Request.Header.Set("X-Session-Id", "shared-image-session")
		tt.serve(c)

		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tt.path, rec.Body.String())
		require.Len(t, logs.rows, i+1, "%s should write exactly one usage row", tt.path)
		require.Len(t, billing.commands, i+1, "%s should submit exactly one billing command", tt.path)
		require.Len(t, upstream.calls, i+1)
		require.Equal(t, chatImageUpstreamCall{path: "/v1/images/generations", body: chatImageBillingDirectBody}, upstream.calls[i])
		row := logs.rows[i]
		require.Equal(t, &tt.wantInbound, row.InboundEndpoint)
		upstreamEndpoint := "/v1/images/generations"
		require.Equal(t, &upstreamEndpoint, row.UpstreamEndpoint)
		require.Equal(t, chatImageModel, row.Model)
		require.Equal(t, chatImageModel, row.RequestedModel)
		require.Equal(t, 2, row.ImageCount)
		require.Equal(t, tt.stream, row.Stream)
		require.Equal(t, service.RequestTypeFromLegacy(tt.stream, false), row.RequestType)
		mode := string(service.BillingModeImage)
		require.Equal(t, &mode, row.BillingMode)
		require.Equal(t, 2*pricePerImage, row.TotalCost)
		require.Equal(t, 2*pricePerImage, row.ActualCost)
		command := billing.commands[i]
		require.Equal(t, service.HashUsageRequestPayload([]byte(chatImageBillingDirectBody)), command.RequestPayloadHash)
		require.Equal(t, 2*pricePerImage, command.BalanceCost)
		if i > 0 {
			direct := logs.rows[0]
			row.InboundEndpoint = direct.InboundEndpoint
			row.Stream, row.RequestType = direct.Stream, direct.RequestType
			row.ID, row.RequestID, row.CreatedAt = direct.ID, direct.RequestID, direct.CreatedAt
			row.DurationMs, row.FirstTokenMs, row.QueueWaitMs = direct.DurationMs, direct.FirstTokenMs, direct.QueueWaitMs
			row.QueueTiming, row.ContentTiming = direct.QueueTiming, direct.ContentTiming
			require.Equal(t, direct, row, "usage row differs for %s", tt.path)
		}
	}
}

func TestOpenAIChatImages_ClientDisconnectStillBills(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stream bool
	}{
		{name: "non_streaming"},
		{name: "streaming", stream: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageBillingResult}
			logs := &chatImageUsageLogs{}
			billing := &compatStreamUsageBillingRepo{}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			h, apiKey := newChatImageHandler(t, cfg, upstream, chatImageBillingCache{newHandlerInflightCache(100)}, logs, billing)
			pricePerImage := 0.125
			apiKey.Group.ImageCountBilling = true
			apiKey.Group.ImagePrice1K = &pricePerImage
			storage := &chatImageStorage{}
			h.imageStorageResolver = func() (*service.ImageResultUploader, bool) {
				return service.NewImageResultUploader(storage, "images/", 1024, nil), true
			}

			direct := httptest.NewRecorder()
			h.Images(newChatImageContext(direct, apiKey, "/v1/images/generations", chatImageBillingDirectBody))
			require.Equal(t, http.StatusOK, direct.Code, direct.Body.String())
			require.Len(t, upstream.calls, 1)
			require.Len(t, logs.rows, 1)
			require.Len(t, billing.commands, 1)
			require.Equal(t, 2*pricePerImage, billing.commands[0].BalanceCost)

			rec := httptest.NewRecorder()
			c := newChatImageContext(rec, apiKey, "/v1/chat/completions", fmt.Sprintf(chatImageBillingChatBody, tt.stream))
			clientCtx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(clientCtx)
			var transportErr error
			upstream.beforeResponse = func(req *http.Request) error {
				cancel()
				transportErr = req.Context().Err()
				return transportErr
			}

			require.NotPanics(t, func() { h.ChatCompletions(c) })

			require.ErrorIs(t, clientCtx.Err(), context.Canceled, "the client must disconnect inside the upstream call")
			require.NoError(t, transportErr, "the in-flight upstream request must survive client cancellation")
			require.Len(t, upstream.calls, 2, "one upstream call for each request")
			require.Equal(t, chatImageUpstreamCall{path: "/v1/images/generations", body: chatImageBillingDirectBody}, upstream.calls[1])
			require.Len(t, logs.rows, 2, "one usage row for each request")
			require.Len(t, billing.commands, 2, "one settlement for each request")
			require.Equal(t, 2, logs.rows[1].ImageCount)
			require.Equal(t, logs.rows[0].ActualCost, logs.rows[1].ActualCost)
			require.Equal(t, billing.commands[0].BalanceCost, billing.commands[1].BalanceCost)
			require.Zero(t, storage.calls)

			if !tt.stream {
				require.Equal(t, statusClientClosedRequest, c.Writer.Status())
				require.False(t, c.Writer.Written())
				require.Empty(t, rec.Body.String())
				return
			}
			frames := testChatImageSSEFrames(t, rec)
			require.Len(t, frames, 1, "only the initial role chunk was written: %s", rec.Body.String())
			require.True(t, strings.HasPrefix(frames[0], "data: "), frames[0])
			var first apicompat.ChatCompletionsChunk
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[0], "data: ")), &first))
			require.Equal(t, "chat.completion.chunk", first.Object)
			require.Equal(t, []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Role: "assistant"}}}, first.Choices)
		})
	}
}

type chatImageBillingCache struct {
	*handlerInflightCache
}

func (c chatImageBillingCache) DeductUserBalance(context.Context, int64, float64) error {
	return nil
}

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
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
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
	status int
	body   string
	wait   <-chan struct{}
	calls  []chatImageUpstreamCall
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
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_chat_image_test"}},
		Body:       io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func newChatImageHandler(t *testing.T, cfg *config.Config, upstream service.HTTPUpstream, cache service.BillingCache, logs service.UsageLogRepository, billing service.UsageBillingRepository) (*OpenAIGatewayHandler, *service.APIKey) {
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
		repo, nil, logs, billing, nil, nil, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
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
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})
	return c
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
			h.ChatCompletions(newChatImageContext(rec, apiKey, "/v1/chat/completions", tt.request))

			testChatImageCompletion(t, rec, chatImageContent)
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
			platform: service.PlatformOpenAI, allowImages: true, stream: true, wantStatus: http.StatusOK, wantCalls: 1, wantError: chatImageError,
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
			name: "Grok keeps the original rejection", request: chatImageRequest,
			platform: service.PlatformGrok, allowImages: true, wantStatus: http.StatusBadRequest,
			wantError: `{"error":{"type":"invalid_request_error","message":"This model is not supported on the Chat Completions endpoint"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &chatImageUpstream{status: http.StatusBadRequest, body: chatImageError}
			h, apiKey := newChatImageHandler(t, &config.Config{RunMode: config.RunModeSimple}, upstream, nil, nil, nil)
			apiKey.Group.Platform = tt.platform
			apiKey.Group.AllowImageGeneration = tt.allowImages
			rec := httptest.NewRecorder()
			h.ChatCompletions(newChatImageContext(rec, apiKey, "/v1/chat/completions", tt.request))

			require.Len(t, upstream.calls, tt.wantCalls)
			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			if tt.stream {
				frames := testChatImageSSEFrames(t, rec)
				require.Len(t, frames, 3, rec.Body.String())
				require.True(t, strings.HasPrefix(frames[0], "data: "), frames[0])
				var first apicompat.ChatCompletionsChunk
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[0], "data: ")), &first))
				require.Equal(t, []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Role: "assistant"}}}, first.Choices)
				require.True(t, strings.HasPrefix(frames[1], "data: "), frames[1])
				require.JSONEq(t, tt.wantError, strings.TrimPrefix(frames[1], "data: "))
				require.Equal(t, "data: [DONE]", frames[2])
			} else {
				require.JSONEq(t, tt.wantError, rec.Body.String())
			}
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
			upstream := &chatImageUpstream{status: http.StatusOK, body: chatImageResult}
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

func TestOpenAIChatImages_BillingMatchesDirectImages(t *testing.T) {
	upstream := &chatImageUpstream{status: http.StatusOK, body: `{"created":1,"data":[{"b64_json":"aGk="},{"b64_json":"eWVz"}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}`}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	logs := &chatImageUsageLogs{}
	billing := &compatStreamUsageBillingRepo{}
	h, apiKey := newChatImageHandler(t, cfg, upstream, chatImageBillingCache{newHandlerInflightCache(100)}, logs, billing)
	apiKey.Group.ImageCountBilling = true
	pricePerImage := 0.125
	apiKey.Group.ImagePrice1K = &pricePerImage
	const directBody = `{"model":"gpt-image-1","n":2,"prompt":"Draw a cat","size":"1024x1024","stream":false}`
	const chatBody = `{"model":"gpt-image-1","n":2,"size":"1024x1024","stream":%t,` + chatImageMessage + `}`

	for i, tt := range []struct {
		path        string
		body        string
		wantInbound string
		serve       gin.HandlerFunc
	}{
		{"/v1/images/generations", directBody, "/v1/images/generations", h.Images},
		{"/v1/chat/completions", fmt.Sprintf(chatBody, false), "/v1/chat/completions", h.ChatCompletions},
		{"/chat/completions", fmt.Sprintf(chatBody, true), "/chat/completions", h.ChatCompletions},
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
		require.Equal(t, chatImageUpstreamCall{path: "/v1/images/generations", body: directBody}, upstream.calls[i])
		row := logs.rows[i]
		require.Equal(t, &tt.wantInbound, row.InboundEndpoint)
		upstreamEndpoint := "/v1/images/generations"
		require.Equal(t, &upstreamEndpoint, row.UpstreamEndpoint)
		require.Equal(t, chatImageModel, row.Model)
		require.Equal(t, chatImageModel, row.RequestedModel)
		require.Equal(t, 2, row.ImageCount)
		require.False(t, row.Stream)
		require.Equal(t, service.RequestTypeSync, row.RequestType)
		mode := string(service.BillingModeImage)
		require.Equal(t, &mode, row.BillingMode)
		require.Equal(t, 2*pricePerImage, row.TotalCost)
		require.Equal(t, 2*pricePerImage, row.ActualCost)
		command := billing.commands[i]
		require.Equal(t, service.HashUsageRequestPayload([]byte(directBody)), command.RequestPayloadHash)
		require.Equal(t, 2*pricePerImage, command.BalanceCost)
		if i > 0 {
			direct := logs.rows[0]
			row.InboundEndpoint = direct.InboundEndpoint
			row.ID, row.RequestID, row.CreatedAt = direct.ID, direct.RequestID, direct.CreatedAt
			row.DurationMs, row.FirstTokenMs, row.QueueWaitMs = direct.DurationMs, direct.FirstTokenMs, direct.QueueWaitMs
			row.QueueTiming, row.ContentTiming = direct.QueueTiming, direct.ContentTiming
			require.Equal(t, direct, row, "usage row differs for %s", tt.path)
		}
	}
}

type chatImageBillingCache struct {
	*handlerInflightCache
}

func (c chatImageBillingCache) DeductUserBalance(context.Context, int64, float64) error {
	return nil
}

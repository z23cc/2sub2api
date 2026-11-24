package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// chatImagesInternalRequestKey marks an Images request run on behalf of a Chat
// Completions request. Images then skips its own JSON keepalive: the recorder
// has no client to keep alive, and a committed 200 there would hide the
// upstream status from the Chat reply.
const chatImagesInternalRequestKey = "chat_images_internal_request"

// chatImagesPlatformSupported reports whether the key's group serves
// gpt-image-* through the OpenAI Images pipeline, the same platform choice the
// /v1/images routes make.
func chatImagesPlatformSupported(c *gin.Context, apiKey *service.APIKey) bool {
	if apiKey == nil || apiKey.Group == nil {
		return false
	}
	platform := apiKey.Group.Platform
	if platform == service.PlatformComposite {
		resolved, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		if !ok {
			return false
		}
		platform = resolved
	}
	return platform == service.PlatformOpenAI
}

// chatCompletionsImages answers a Chat Completions request for a gpt-image-*
// model by running the Images pipeline internally (group gate, audit, image
// slot, account selection, failover and per-image billing) and replying with
// the images as markdown.
func (h *OpenAIGatewayHandler) chatCompletionsImages(c *gin.Context, reqLog *zap.Logger, body []byte, reqModel string, reqStream bool, streamStarted *bool) {
	imagesReq, err := apicompat.ChatCompletionsToImagesRequest(body)
	if err != nil {
		var reqErr *apicompat.ChatImagesRequestError
		if !errors.As(err, &reqErr) {
			reqLog.Error("openai_chat_completions.images_request_conversion_failed", zap.Error(err))
			h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to build image request")
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", reqErr.Message)
		return
	}
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream), zap.Bool("chat_images_edits", imagesReq.Edits))
	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	inboundEndpoint := GetInboundEndpoint(c)
	imagesPath, upstreamEndpoint := chatImagesPath(c.Request.URL.Path, imagesReq.Edits)
	// The client's own context: a disconnect cancels generation, as on /images.
	inner, recorder := newInternalImageContext(c, c.Request.Context(), imagesPath, imagesReq.Body)
	inner.Request.Header.Set("Content-Type", "application/json")
	inner.Request.Header.Set("Accept", "application/json")
	inner.Request.Header.Del("Content-Encoding")
	inner.Request.Header.Del("Content-Length")
	inner.Set(chatImagesInternalRequestKey, true)
	// Usage and ops keep the Chat entry point, while the upstream endpoint
	// matches a direct Images request.
	inner.Set(ctxKeyInboundEndpoint, inboundEndpoint)
	setActualUpstreamEndpoint(inner, upstreamEndpoint)

	id := apicompat.NewChatImageCompletionID()
	created := time.Now().Unix()

	// Keepalives write the client writer while Images writes the recorder.
	// They start after the copy above so the inner context cannot see them.
	var sse *chatImagesSSE
	if reqStream {
		sse = startChatImagesSSE(c, apicompat.ChatImageRoleChunk(id, reqModel, created), h.chatImagesStreamKeepaliveInterval())
		*streamStarted = true
		defer sse.stop()
	} else {
		restoreWriter := service.StartOpenAIImagesJSONKeepalive(c, h.openAIImagesJSONKeepaliveInterval())
		defer restoreWriter()
	}

	h.Images(inner)

	// Images has returned, so the recorder and inner keys are safe to read.
	// The keepalive keeps running through the object storage upload below,
	// which reads only the recorded result and never writes the client.
	mergeChatImagesKeys(c, inner)
	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	outcome := readInternalImageOutcome(inner, recorder)
	var content string
	var usage *apicompat.ChatUsage
	if outcome.Succeeded() {
		result := h.offloadChatImages(c, reqLog, outcome.Body)
		var err error
		content, usage, err = apicompat.ImagesResultToChatContent(result, imagesReq.OutputFormat)
		if err != nil {
			// Images bills only images it counted, so an empty result was not charged.
			reqLog.Warn("openai_chat_completions.images_result_unrenderable", zap.Error(err))
			outcome = internalImageOutcome{StatusCode: http.StatusBadGateway, InvalidSuccess: true}
		}
	}

	jsonCommitted := false
	if sse != nil {
		sse.stop()
	} else {
		jsonCommitted = service.StopOpenAIImagesJSONKeepaliveCommitted(c)
	}

	if outcome.ContextDone {
		reqLog.Info("openai_chat_completions.images_client_gone")
		return
	}
	if !outcome.Succeeded() {
		h.writeChatImagesError(c, sse, recorder, outcome, jsonCommitted)
		return
	}

	if sse == nil {
		c.JSON(http.StatusOK, apicompat.NewChatImageCompletion(id, reqModel, created, content, usage))
		return
	}
	sse.writeChunk(apicompat.ChatImageContentChunk(id, reqModel, created, content))
	sse.writeChunk(apicompat.ChatImageFinishChunk(id, reqModel, created))
	if imagesReq.IncludeUsage {
		sse.writeChunk(apicompat.ChatImageUsageChunk(id, reqModel, created, usage))
	}
	sse.writeDone()
}

// chatImagesPath maps the Chat path onto the Images path with the same prefix
// (/v1/chat/completions -> /v1/images/generations) and returns the canonical
// upstream endpoint a direct Images request would record.
func chatImagesPath(chatPath string, edits bool) (string, string) {
	prefix := strings.TrimSuffix(strings.TrimRight(chatPath, "/"), "/chat/completions")
	if edits {
		return prefix + "/images/edits", EndpointImagesEdits
	}
	return prefix + "/images/generations", EndpointImagesGenerations
}

// mergeChatImagesKeys copies what Images recorded on its context (selected
// account, upstream errors, latencies) back so the outer request's ops log
// describes the image attempt.
func mergeChatImagesKeys(c, inner *gin.Context) {
	for key, value := range inner.Keys {
		c.Set(key, value)
	}
}

// offloadChatImages stores the images in object storage when it is enabled
// and returns the rewritten result. The images are already generated and
// billed, so a storage failure falls back to the original result.
func (h *OpenAIGatewayHandler) offloadChatImages(c *gin.Context, reqLog *zap.Logger, result []byte) []byte {
	if h.imageStorageResolver == nil {
		return result
	}
	uploader, enabled := h.imageStorageResolver()
	if !enabled || uploader == nil {
		return result
	}
	objectID := "chatimg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	rewritten, err := uploader.Rewrite(c.Request.Context(), objectID, result)
	if err != nil {
		reqLog.Warn("openai_chat_completions.images_offload_failed", zap.String("object_id", objectID), zap.Error(err))
		return result
	}
	return rewritten
}

// writeChatImagesError reports a failed image attempt. Before the stream
// starts it repeats the Images status and OpenAI error body; once it has
// started, it sends an in-band error followed by [DONE].
func (h *OpenAIGatewayHandler) writeChatImagesError(c *gin.Context, sse *chatImagesSSE, recorder *httptest.ResponseRecorder, outcome internalImageOutcome, jsonCommitted bool) {
	errorObject := outcome.ErrorObject()
	payload, err := json.Marshal(map[string]json.RawMessage{"error": errorObject})
	if err != nil {
		payload = []byte(`{"error":{"type":"api_error","message":"image generation failed"}}`)
	}
	var detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(errorObject, &detail)

	if sse != nil {
		service.MarkOpsStreamError(c, detail.Type, detail.Message, outcome.StatusCode)
		sse.writeData(payload)
		sse.writeDone()
		return
	}
	status := outcome.StatusCode
	if jsonCommitted {
		// A whitespace keepalive already committed 200, as on /images.
		status = http.StatusOK
	} else if retryAfter := recorder.Header().Get("Retry-After"); retryAfter != "" {
		c.Header("Retry-After", retryAfter)
	}
	c.Data(status, "application/json; charset=utf-8", payload)
}

func (h *OpenAIGatewayHandler) chatImagesStreamKeepaliveInterval() time.Duration {
	if h.cfg == nil || h.cfg.Gateway.ImageStreamKeepaliveInterval <= 0 {
		return 0
	}
	return time.Duration(h.cfg.Gateway.ImageStreamKeepaliveInterval) * time.Second
}

// chatImagesSSE writes the Chat stream to the client. Its keepalive goroutine
// and the handler share the writer through mu; stop waits for the goroutine.
type chatImagesSSE struct {
	c        *gin.Context
	mu       sync.Mutex
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// startChatImagesSSE commits the stream, sends first, then sends
// ": keepalive" comments every interval until stop. A non-positive interval
// disables the comments.
func startChatImagesSSE(c *gin.Context, first *apicompat.ChatCompletionsChunk, interval time.Duration) *chatImagesSSE {
	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	s := &chatImagesSSE{c: c, quit: make(chan struct{}), done: make(chan struct{})}
	s.writeChunk(first)
	if interval <= 0 {
		close(s.done)
		return s
	}
	reqDone := c.Request.Context().Done()
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.quit:
				return
			case <-reqDone:
				return
			case <-ticker.C:
			}
			if !s.write([]byte(": keepalive\n\n")) {
				return
			}
		}
	}()
	return s
}

func (s *chatImagesSSE) stop() {
	s.stopOnce.Do(func() {
		close(s.quit)
		<-s.done
	})
}

func (s *chatImagesSSE) write(data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.c.Writer.Write(data); err != nil {
		return false
	}
	s.c.Writer.Flush()
	return true
}

func (s *chatImagesSSE) writeData(payload []byte) {
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, "\n\n"...)
	s.write(frame)
}

func (s *chatImagesSSE) writeChunk(chunk *apicompat.ChatCompletionsChunk) {
	payload, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	s.writeData(payload)
}

func (s *chatImagesSSE) writeDone() {
	s.write([]byte("data: [DONE]\n\n"))
}

//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestReadInternalImageOutcome(t *testing.T) {
	const invalid = `{"type":"api_error","message":"upstream returned an invalid image response"}`
	const detail = `{"type":"invalid_request_error","code":"invalid_value","message":"bad size"}`
	const envelope = `{"error":` + detail + `}`
	tests := []struct {
		name           string
		recorderStatus int
		auditStatus    int
		body           string
		cancelled      bool
		wantStatus     int
		contextDone    bool
		invalidSuccess bool
		succeeded      bool
		errorObject    string
	}{
		{name: "canceled with only whitespace", body: " \n", cancelled: true, wantStatus: 504, contextDone: true},
		{name: "audit status overrides committed 200", recorderStatus: 200, auditStatus: 403, body: envelope, wantStatus: 403, errorObject: detail},
		{name: "empty success", recorderStatus: 204, wantStatus: 502, invalidSuccess: true, errorObject: invalid},
		{name: "non-JSON success", recorderStatus: 200, body: "oops", wantStatus: 502, invalidSuccess: true, errorObject: invalid},
		{name: "JSON success despite lower audit status", recorderStatus: 201, auditStatus: 302, body: " \n{\"data\":[]}\n", wantStatus: 201, succeeded: true},
		{name: "zero recorder status with JSON", body: `{"data":[]}`, wantStatus: 200, succeeded: true},
		{name: "client error", recorderStatus: 400, body: envelope, wantStatus: 400, errorObject: detail},
		{name: "server error", recorderStatus: 503, body: `{"error":{"type":"server_error","message":"unavailable"}}`, wantStatus: 503, errorObject: `{"type":"server_error","message":"unavailable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			inner, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			inner.Request = httptest.NewRequest(http.MethodPost, EndpointImagesGenerations, nil).WithContext(ctx)
			if tt.auditStatus != 0 {
				inner.Set(upstreamAuditFailureStatusKey, tt.auditStatus)
			}
			recorder.Code = tt.recorderStatus
			_, err := recorder.Body.WriteString(tt.body)
			require.NoError(t, err)

			outcome := readInternalImageOutcome(inner, recorder)
			require.Equal(t, tt.wantStatus, outcome.StatusCode)
			require.Equal(t, tt.contextDone, outcome.ContextDone)
			require.Equal(t, tt.invalidSuccess, outcome.InvalidSuccess)
			require.Equal(t, tt.succeeded, outcome.Succeeded())
			require.Equal(t, strings.TrimSpace(tt.body), string(outcome.Body))
			if tt.errorObject != "" {
				require.JSONEq(t, tt.errorObject, string(outcome.ErrorObject()))
			}
		})
	}
}

func TestInternalImageContexts_CancellationAndRequestPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outer, _ := gin.CreateTestContext(httptest.NewRecorder())
	outer.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations/async", nil).WithContext(ctx)
	outer.Set(ctxKeyInboundEndpoint, EndpointImagesGenerations)
	body := []byte(`{"model":"gpt-image-1","prompt":"Draw a cat"}`)

	task, _, cancelTask, err := newAsyncImageContext(outer, body, time.Minute)
	require.NoError(t, err)
	defer cancelTask()
	require.Equal(t, "/v1/images/generations", task.Request.URL.Path)

	chat, _ := newInternalImageContext(outer, ctx, "/v1/images/edits", body)
	require.Equal(t, "/v1/images/edits", chat.Request.URL.Path)
	require.Equal(t, "/v1/images/generations/async", outer.Request.URL.Path)
	require.Equal(t, EndpointImagesGenerations, GetInboundEndpoint(chat))
	require.Equal(t, int64(len(body)), chat.Request.ContentLength)
	replay, err := chat.Request.GetBody()
	require.NoError(t, err)
	defer func() { require.NoError(t, replay.Close()) }()
	gotBody, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, body, gotBody)

	cancel()
	require.ErrorIs(t, chat.Request.Context().Err(), context.Canceled)
	require.NoError(t, task.Request.Context().Err())
	cancelTask()
	require.ErrorIs(t, task.Request.Context().Err(), context.Canceled)
}

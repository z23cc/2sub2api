//go:build unit

package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatCompletionsToImagesRequest(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantBody  string
		edits     bool
		format    string
		usage     bool
		wantError string
	}{
		{
			name: "string content, last user only, and typed image options",
			body: `{
				"model":"gpt-image-1","stream":true,"stream_options":{"include_usage":true},
				"size":"1024x1024","quality":"high","n":2,"background":"transparent",
				"output_format":"webp","output_compression":75,"moderation":"low","temperature":0.1,
				"messages":[
					{"role":"user","content":[{"type":"text","text":"old prompt"},{"type":"image_url","image_url":"https://example.com/old.png"}]},
					{"role":"system","content":"system instruction"},
					{"role":"user","content":" Draw a cat "},
					{"role":"assistant","content":"assistant reply"}
				]
			}`,
			wantBody: `{"model":"gpt-image-1","prompt":"Draw a cat","stream":false,"size":"1024x1024","quality":"high","n":2,"background":"transparent","output_format":"webp","output_compression":75,"moderation":"low"}`,
			format:   "webp",
			usage:    true,
		},
		{
			name:     "text parts with explicit false include_usage",
			body:     `{"model":"gpt-image-1","stream_options":{"include_usage":false},"messages":[{"role":"user","content":[{"type":"text","text":" First "},{"type":"text","text":"  "},{"type":"text","text":" Second "}]}]}`,
			wantBody: `{"model":"gpt-image-1","prompt":"First\nSecond","stream":false}`,
		},
		{
			name:     "edits with an HTTP image object and a data URL string",
			body:     `{"model":"gpt-image-1","stream":false,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}},{"type":"text","text":"Add stripes"},{"type":"image_url","image_url":"data:image/png;base64,aGk="}]}]}`,
			wantBody: `{"model":"gpt-image-1","prompt":"Add stripes","stream":false,"images":[{"image_url":"https://example.com/cat.png"},{"image_url":"data:image/png;base64,aGk="}]}`,
			edits:    true,
		},
		{
			name:      "empty last prompt ignores previous prompt",
			body:      `{"model":"gpt-image-1","messages":[{"role":"user","content":"Earlier prompt"},{"role":"user","content":"  "}]}`,
			wantError: "the last user message must contain a text prompt",
		},
		{
			name:      "image without text",
			body:      `{"model":"gpt-image-1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`,
			wantError: "the last user message must contain a text prompt",
		},
		{
			name:      "no user message",
			body:      `{"model":"gpt-image-1","messages":[{"role":"system","content":"Draw a cat"}]}`,
			wantError: "the last user message must contain a text prompt",
		},
		{
			name:      "invalid JSON",
			body:      `{"model":`,
			wantError: "request body is not valid JSON",
		},
		{
			name:      "missing model",
			body:      `{"messages":[{"role":"user","content":"Draw a cat"}]}`,
			wantError: "model is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChatCompletionsToImagesRequest([]byte(tt.body))
			if tt.wantError != "" {
				var requestErr *ChatImagesRequestError
				require.ErrorAs(t, err, &requestErr)
				require.Equal(t, tt.wantError, requestErr.Message)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.JSONEq(t, tt.wantBody, string(got.Body))
			require.Equal(t, tt.edits, got.Edits)
			require.Equal(t, tt.format, got.OutputFormat)
			require.Equal(t, tt.usage, got.IncludeUsage)
		})
	}
}

func TestImagesResultToChatContent(t *testing.T) {
	tests := []struct {
		name      string
		result    string
		requested string
		content   string
		usage     *ChatUsage
		wantError string
	}{
		{
			name:      "response format, multiple images, URL precedence and reported total",
			result:    `{"output_format":"jpeg","data":[{"b64_json":"aGk="},{"url":"https://example.com/cat.png","b64_json":"aGk="},{"b64_json":"eWVz"}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":16}}`,
			requested: "webp",
			content:   "![image](data:image/jpeg;base64,aGk=)\n\n![image](https://example.com/cat.png)\n\n![image](data:image/jpeg;base64,eWVz)",
			usage:     &ChatUsage{PromptTokens: 5, CompletionTokens: 10, TotalTokens: 16},
		},
		{
			name:      "request format fallback and computed total",
			result:    `{"data":[{"b64_json":"aGk="}],"usage":{"input_tokens":5,"output_tokens":10}}`,
			requested: "webp",
			content:   "![image](data:image/webp;base64,aGk=)",
			usage:     &ChatUsage{PromptTokens: 5, CompletionTokens: 10, TotalTokens: 15},
		},
		{
			name:    "default PNG and absent usage",
			result:  `{"data":[{"b64_json":"aGk="}]}`,
			content: "![image](data:image/png;base64,aGk=)",
		},
		{
			name:      "no usable image",
			result:    `{"data":[{},{"url":" ","b64_json":""}]}`,
			wantError: "image response contains no images",
		},
		{
			name:      "invalid JSON",
			result:    "bad JSON",
			wantError: "image response is not valid JSON",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, usage, err := ImagesResultToChatContent([]byte(tt.result), tt.requested)
			if tt.wantError != "" {
				require.EqualError(t, err, tt.wantError)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.content, content)
			require.Equal(t, tt.usage, usage)
		})
	}
}

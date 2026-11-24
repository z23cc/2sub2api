package apicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// chatImagesPassthroughFields are non-standard top-level Chat fields that
// image clients send through the OpenAI SDK's extra_body.
var chatImagesPassthroughFields = []string{
	"size",
	"quality",
	"n",
	"background",
	"output_format",
	"output_compression",
	"moderation",
}

// ChatImagesRequestError is a Chat request that cannot become an Images
// request; callers answer it with 400 invalid_request_error.
type ChatImagesRequestError struct {
	Message string
}

func (e *ChatImagesRequestError) Error() string { return e.Message }

// ChatImagesRequest is a Chat Completions request rewritten for the Images API.
type ChatImagesRequest struct {
	// Edits is true when the last user message carries images, which selects
	// /v1/images/edits instead of /v1/images/generations.
	Edits bool
	// Body is the Images JSON request body. It always sets stream=false.
	Body []byte
	// OutputFormat is the client's requested output_format, if any.
	OutputFormat string
	// IncludeUsage mirrors stream_options.include_usage.
	IncludeUsage bool
}

// ChatCompletionsToImagesRequest converts a Chat Completions body for a
// gpt-image-* model into an Images request. Only the last user message is
// used: its text parts form the prompt and its image_url parts become edit
// inputs. System messages and earlier turns are ignored.
func ChatCompletionsToImagesRequest(body []byte) (*ChatImagesRequest, error) {
	if !gjson.ValidBytes(body) {
		return nil, &ChatImagesRequestError{Message: "request body is not valid JSON"}
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return nil, &ChatImagesRequestError{Message: "model is required"}
	}

	var lastUser gjson.Result
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		items := messages.Array()
		for i := len(items) - 1; i >= 0; i-- {
			if strings.EqualFold(strings.TrimSpace(items[i].Get("role").String()), "user") {
				lastUser = items[i]
				break
			}
		}
	}
	prompt, imageURLs, err := chatImagesUserInput(lastUser.Get("content"))
	if err != nil {
		return nil, err
	}
	if prompt == "" {
		return nil, &ChatImagesRequestError{Message: "the last user message must contain a text prompt"}
	}

	out := map[string]json.RawMessage{}
	setString := func(key, value string) {
		raw, _ := json.Marshal(value)
		out[key] = raw
	}
	setString("model", model)
	setString("prompt", prompt)
	out["stream"] = json.RawMessage("false")
	for _, field := range chatImagesPassthroughFields {
		if value := gjson.GetBytes(body, field); value.Exists() && value.Type != gjson.Null {
			out[field] = json.RawMessage(value.Raw)
		}
	}
	if len(imageURLs) > 0 {
		images := make([]map[string]string, 0, len(imageURLs))
		for _, imageURL := range imageURLs {
			images = append(images, map[string]string{"image_url": imageURL})
		}
		raw, err := json.Marshal(images)
		if err != nil {
			return nil, fmt.Errorf("encode images: %w", err)
		}
		out["images"] = raw
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode images request: %w", err)
	}

	outputFormat := ""
	if value := gjson.GetBytes(body, "output_format"); value.Type == gjson.String {
		outputFormat = strings.TrimSpace(value.String())
	}
	return &ChatImagesRequest{
		Edits:        len(imageURLs) > 0,
		Body:         encoded,
		OutputFormat: outputFormat,
		IncludeUsage: gjson.GetBytes(body, "stream_options.include_usage").Bool(),
	}, nil
}

func chatImagesUserInput(content gjson.Result) (string, []string, error) {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()), nil, nil
	}
	if !content.IsArray() {
		return "", nil, nil
	}
	var texts, imageURLs []string
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "text":
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
		case "image_url":
			imageURL := part.Get("image_url")
			if imageURL.IsObject() {
				imageURL = imageURL.Get("url")
			}
			value := ""
			if imageURL.Type == gjson.String {
				value = strings.TrimSpace(imageURL.String())
			}
			// Dropping it would silently turn an edit into a billed generation,
			// or edit with only some of the input images.
			if value == "" {
				return "", nil, &ChatImagesRequestError{Message: "image_url content parts must include a non-empty url"}
			}
			imageURLs = append(imageURLs, value)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n")), imageURLs, nil
}

// ErrChatImagesNoImages reports a valid Images response without any image.
var ErrChatImagesNoImages = errors.New("image response contains no images")

// ImagesResultToChatContent renders an Images API response as markdown image
// content and maps its usage. b64_json items become data URLs typed by the
// response's output_format, then defaultOutputFormat, then png; items without
// b64_json use their url. b64 wins because an upstream url may expire or be
// unreachable for the client, while object storage offload removes b64_json.
// Several images are separated by a blank line. A response without images
// returns ErrChatImagesNoImages together with its usage.
func ImagesResultToChatContent(result []byte, defaultOutputFormat string) (string, *ChatUsage, error) {
	if !gjson.ValidBytes(result) {
		return "", nil, errors.New("image response is not valid JSON")
	}
	outputFormat := strings.TrimSpace(gjson.GetBytes(result, "output_format").String())
	if outputFormat == "" {
		outputFormat = strings.TrimSpace(defaultOutputFormat)
	}
	mimeType := chatImagesMIMEType(outputFormat)

	var images []string
	for _, item := range gjson.GetBytes(result, "data").Array() {
		if b64 := strings.TrimSpace(item.Get("b64_json").String()); b64 != "" {
			images = append(images, fmt.Sprintf("![image](data:%s;base64,%s)", mimeType, b64))
			continue
		}
		if imageURL := strings.TrimSpace(item.Get("url").String()); imageURL != "" {
			images = append(images, "![image]("+imageURL+")")
		}
	}
	usage := chatImagesUsage(gjson.GetBytes(result, "usage"))
	if len(images) == 0 {
		return "", usage, ErrChatImagesNoImages
	}
	return strings.Join(images, "\n\n"), usage, nil
}

func chatImagesMIMEType(outputFormat string) string {
	switch strings.ToLower(outputFormat) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func chatImagesUsage(usage gjson.Result) *ChatUsage {
	if !usage.IsObject() {
		return nil
	}
	input := int(usage.Get("input_tokens").Int())
	output := int(usage.Get("output_tokens").Int())
	total := int(usage.Get("total_tokens").Int())
	if total == 0 {
		total = input + output
	}
	return &ChatUsage{PromptTokens: input, CompletionTokens: output, TotalTokens: total}
}

// NewChatImageCompletion builds the non-streaming Chat response for a
// generated image.
func NewChatImageCompletion(id, model string, created int64, content string, usage *ChatUsage) *ChatCompletionsResponse {
	raw, _ := json.Marshal(content)
	return &ChatCompletionsResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      ChatMessage{Role: "assistant", Content: raw},
			FinishReason: "stop",
		}},
		Usage: usage,
	}
}

// ChatImageRoleChunk is the first streaming chunk, sent before generation.
func ChatImageRoleChunk(id, model string, created int64) *ChatCompletionsChunk {
	return chatImageChunk(id, model, created, ChatDelta{Role: "assistant"}, nil)
}

// ChatImageContentChunk carries the whole markdown content in one delta.
func ChatImageContentChunk(id, model string, created int64, content string) *ChatCompletionsChunk {
	return chatImageChunk(id, model, created, ChatDelta{Content: &content}, nil)
}

// ChatImageFinishChunk ends the choice with finish_reason "stop".
func ChatImageFinishChunk(id, model string, created int64) *ChatCompletionsChunk {
	stop := "stop"
	return chatImageChunk(id, model, created, ChatDelta{}, &stop)
}

// ChatImageUsageChunk is the stream_options.include_usage chunk; it carries an
// empty choices array as OpenAI does.
func ChatImageUsageChunk(id, model string, created int64, usage *ChatUsage) *ChatCompletionsChunk {
	if usage == nil {
		usage = &ChatUsage{}
	}
	return &ChatCompletionsChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChatChunkChoice{},
		Usage:   usage,
	}
}

func chatImageChunk(id, model string, created int64, delta ChatDelta, finishReason *string) *ChatCompletionsChunk {
	return &ChatCompletionsChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChatChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
}

// NewChatImageCompletionID returns a chat completion id for an image reply.
func NewChatImageCompletionID() string {
	return generateChatCmplID()
}

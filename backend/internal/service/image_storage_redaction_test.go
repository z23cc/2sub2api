//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageResultUploaderRedactsDownloadErrorURL(t *testing.T) {
	transportErr := errors.New("test download failed")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	})}
	storage := &fakeImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 1024, client)
	result := json.RawMessage(`{"data":[{"url":"https://user:pass@host/x.png?X-Amz-Signature=secret#frag"}]}`)
	_, err := uploader.Rewrite(t.Context(), "test-image", result)

	require.ErrorIs(t, err, transportErr)
	require.Contains(t, err.Error(), "https://host/x.png")
	for _, secret := range []string{"secret", "X-Amz", "user:pass", "frag"} {
		require.NotContains(t, err.Error(), secret)
	}
	require.Empty(t, storage.saved)
}

func TestRedactImageDownloadURL(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		want string
	}{
		{name: "userinfo query and fragment", url: "https://user:pass@host/x.png?X-Amz-Signature=secret#frag", want: "https://host/x.png"},
		{name: "port and escaped path", url: "https://host:8443/a%2Fb.png?secret", want: "https://host:8443/a%2Fb.png"},
		{name: "empty query", url: "https://host/x.png?", want: "https://host/x.png"},
		{name: "already safe", url: "https://host/path/x.png", want: "https://host/path/x.png"},
		{name: "malformed URL", url: "https://%xx/path?secret", want: "[redacted url]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, redactImageDownloadURL(tt.url))
		})
	}
}

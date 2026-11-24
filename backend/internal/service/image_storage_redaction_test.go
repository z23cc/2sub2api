//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestImageResultUploaderRedactsMalformedRedirectLocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://h/%zz/x?sig=SECRET#frag")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	storage := &fakeImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 0, server.Client())
	result := json.RawMessage(fmt.Sprintf(`{"data":[{"url":%q}]}`, server.URL+"/start"))
	_, err := uploader.Rewrite(t.Context(), "test-image", result)

	require.ErrorContains(t, err, "failed to parse Location header")
	require.ErrorContains(t, err, "https://h/%zz/x")
	require.NotContains(t, err.Error(), "SECRET")
	require.NotContains(t, err.Error(), "sig=")
	require.NotContains(t, err.Error(), "#frag")
	var original *url.Error
	require.ErrorAs(t, err, &original)
	require.Contains(t, original.Error(), "SECRET", "the original error remains available to callers")
	require.Empty(t, storage.saved)
}

func TestRedactImageDownloadError(t *testing.T) {
	cause := errors.New("download failed")
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "userinfo query and fragment",
			err:  &url.Error{Op: "Get", URL: "https://u:p@host/x.png?X-Amz-Signature=signed-value#frag", Err: cause},
			want: `Get "https://host/x.png": download failed`,
		},
		{
			name: "nested redirect Location",
			err: &url.Error{Op: "Get", URL: "https://host/start?sig=signed-value", Err: fmt.Errorf(
				"failed to parse Location header %q: %w", "https://u:p@h/x?X-Amz-Signature=signed-value#frag", cause)},
			want: `Get "https://host/start": failed to parse Location header "https://h/x": download failed`,
		},
		{
			name: "scheme-relative",
			err:  &url.Error{Op: "Get", URL: "//host/p?sig=signed-value#frag", Err: cause},
			want: `Get "//host/p": download failed`,
		},
		{
			name: "relative path",
			err:  &url.Error{Op: "Get", URL: "/p?sig=signed-value#frag", Err: cause},
			want: `Get "/p": download failed`,
		},
		{name: "ordinary error", err: cause, want: "download failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			redacted := redactImageDownloadError(tt.err)
			require.EqualError(t, redacted, tt.want)
			for _, sensitive := range []string{"X-Amz", "signed-value", "u:p", "#f"} {
				require.NotContains(t, redacted.Error(), sensitive)
			}
			require.ErrorIs(t, redacted, tt.err)
			require.ErrorIs(t, redacted, cause)
			var original *url.Error
			if want, ok := tt.err.(*url.Error); ok {
				require.ErrorAs(t, redacted, &original)
				require.Same(t, want, original)
			} else {
				require.False(t, errors.As(redacted, &original))
			}
		})
	}
}

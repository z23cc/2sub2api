//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// signedImageURLSecrets are parts of a signed URL that must never reach a log.
var signedImageURLSecrets = []string{"SECRET", "?", "sig=", "user:pass", "frag", "a'b"}

func requireNoSignedURLParts(t *testing.T, err error) {
	t.Helper()
	for _, secret := range signedImageURLSecrets {
		require.NotContains(t, err.Error(), secret)
	}
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout reading https://host/x.png?sig=SECRET" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

func TestImageResultUploaderRedactsDownloadErrorURL(t *testing.T) {
	transportErr := errors.New("test download failed for https://host/x.png?name=a'b&sig=SECRET")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	})}
	storage := &fakeImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 1024, client)
	result := json.RawMessage(`{"data":[{"url":"https://user:pass@host/x.png?name=a'b&sig=SECRET#frag"}]}`)
	_, err := uploader.Rewrite(t.Context(), "test-image", result)

	require.Error(t, err)
	require.Contains(t, err.Error(), "request to https://host failed (other)")
	requireNoSignedURLParts(t, err)
	require.ErrorIs(t, err, transportErr)
	var original *url.Error
	require.ErrorAs(t, err, &original)
	require.Contains(t, original.URL, "sig=SECRET", "the original error remains available to callers")
	require.Empty(t, storage.saved)
}

func TestImageResultUploaderRedactsMalformedRedirectLocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://h/%zz/x?name=a'b&sig=SECRET#frag")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	storage := &fakeImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 0, server.Client())
	result := json.RawMessage(fmt.Sprintf(`{"data":[{"url":%q}]}`, server.URL+"/start?sig=SECRET"))
	_, err := uploader.Rewrite(t.Context(), "test-image", result)

	require.Error(t, err)
	require.Contains(t, err.Error(), "request to "+server.URL+" failed (other)")
	requireNoSignedURLParts(t, err)
	var original *url.Error
	require.ErrorAs(t, err, &original)
	require.Contains(t, original.Error(), "failed to parse Location header")
	require.Contains(t, original.Error(), "SECRET", "the original error remains available to callers")
	require.Empty(t, storage.saved)
}

func TestImageResultUploaderRedactsConnectionError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	storage := &fakeImageStorage{}
	uploader := NewImageResultUploader(storage, "images/", 0, &http.Client{})
	result := json.RawMessage(fmt.Sprintf(`{"data":[{"url":"http://%s/x.png?name=a'b&sig=SECRET"}]}`, addr))
	_, err = uploader.Rewrite(t.Context(), "test-image", result)

	require.Error(t, err)
	require.Contains(t, err.Error(), "request to http://"+addr+" failed (connection)")
	requireNoSignedURLParts(t, err)
	var opErr *net.OpError
	require.ErrorAs(t, err, &opErr)
	require.Empty(t, storage.saved)
}

func TestImageResultUploaderRedactsTimeout(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, timeoutNetError{}
	})}
	uploader := NewImageResultUploader(&fakeImageStorage{}, "images/", 0, client)
	result := json.RawMessage(`{"data":[{"url":"https://host/x.png?name=a'b&sig=SECRET"}]}`)
	_, err := uploader.Rewrite(t.Context(), "test-image", result)

	require.Error(t, err)
	require.Contains(t, err.Error(), "request to https://host failed (timeout)")
	requireNoSignedURLParts(t, err)
	require.ErrorIs(t, err, timeoutNetError{})
	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	require.True(t, netErr.Timeout())
}

func TestRedactImageDownloadError(t *testing.T) {
	const signedURL = "https://user:pass@host:8443/x.png?name=a'b&sig=SECRET#frag"
	parseErr := func() error {
		_, err := http.NewRequest(http.MethodGet, "https://host/x\x7f.png?sig=SECRET", nil)
		require.Error(t, err)
		return err
	}()
	canceledErr := &url.Error{Op: "Get", URL: signedURL, Err: context.Canceled}
	deadlineErr := &url.Error{Op: "Get", URL: signedURL, Err: fmt.Errorf("waiting: %w", context.DeadlineExceeded)}
	timeoutErr := &url.Error{Op: "Get", URL: signedURL, Err: timeoutNetError{}}
	dialErr := &url.Error{Op: "Get", URL: signedURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused to " + signedURL)}}
	dnsErr := &url.Error{Op: "Get", URL: signedURL, Err: &net.DNSError{Err: "no such host", Name: "host"}}
	redirectErr := &url.Error{Op: "Get", URL: signedURL, Err: fmt.Errorf("failed to parse Location header %q: %v", signedURL, errors.New("invalid URL escape"))}
	plainErr := errors.New("download failed: " + signedURL)

	for _, tt := range []struct {
		name   string
		rawURL string
		err    error
		want   string
	}{
		{"single quote in query", signedURL, plainErr, "request to https://host:8443 failed (other)"},
		{"canceled", signedURL, canceledErr, "request to https://host:8443 failed (canceled)"},
		{"deadline", signedURL, deadlineErr, "request to https://host:8443 failed (timeout)"},
		{"net timeout", signedURL, timeoutErr, "request to https://host:8443 failed (timeout)"},
		{"invalid url", "https://host/x\x7f.png?sig=SECRET", parseErr, "request to unknown host failed (invalid_url)"},
		{"connection", signedURL, dialErr, "request to https://host:8443 failed (connection)"},
		{"dns", signedURL, dnsErr, "request to https://host:8443 failed (connection)"},
		{"malformed redirect location", signedURL, redirectErr, "request to https://host:8443 failed (other)"},
		{"relative url", "/x.png?sig=SECRET", plainErr, "request to unknown host failed (other)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			redacted := redactImageDownloadError(tt.rawURL, tt.err)
			require.EqualError(t, redacted, tt.want)
			requireNoSignedURLParts(t, redacted)
			require.ErrorIs(t, redacted, tt.err)
			require.Same(t, tt.err, errors.Unwrap(redacted))
			if original, ok := tt.err.(*url.Error); ok {
				var got *url.Error
				require.ErrorAs(t, redacted, &got)
				require.Same(t, original, got)
			}
			// zap prints an error through fmt.Formatter when one is present.
			_, isFormatter := redacted.(fmt.Formatter)
			require.False(t, isFormatter)
			require.False(t, strings.Contains(fmt.Sprintf("%+v", redacted), "SECRET"))
		})
	}
	require.NoError(t, redactImageDownloadError(signedURL, nil))
}

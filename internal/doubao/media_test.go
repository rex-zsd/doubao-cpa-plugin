package doubao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMediaSignerMatchesIndependentVector(t *testing.T) {
	q := url.Values{"Action": {"ApplyImageUpload"}, "FileExtension": {".png"}, "ServiceId": {"test service"}, "Version": {"2018-08-01"}}
	h := mediaSign("GET", q, nil, mediaCredentials{"test-access", "test-secret", "test-token"}, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	if !strings.HasSuffix(h.Get("Authorization"), "Signature=4a8c6aad49b469680167475a7f0e3105c1c5be754c751af1f9f74584be84d7a6") {
		t.Fatal("signature does not match independent Python reference")
	}
	if strings.Contains(mediaQuery(q), "+") || !strings.Contains(mediaQuery(q), "test%20service") {
		t.Fatal("noncanonical query")
	}
	for _, bad := range []string{"http://example.com", "https://example.com:443", "https://user@example.com", "https://example.com/path"} {
		if _, err := mediaOrigin(bad); err == nil {
			t.Fatal("unsafe upload origin", bad)
		}
	}
}

func TestMediaRedirectNeverForwardsUploadCredentials(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("account cookie sent to storage")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, err := mediaJSON(context.Background(), "POST", source.URL, http.Header{"Authorization": {"synthetic-upload-token"}}, []byte("fixture"))
	if err == nil || called {
		t.Fatal("followed upload redirect")
	}
}

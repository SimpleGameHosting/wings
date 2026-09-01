package modpackinstall

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadHappyPathAndExactLength(t *testing.T) {
	body := "archive-bytes-here"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	fs := newTestFs(t)
	n, err := Download(context.Background(), fs, srv.URL+"/pack.tar.gz?sig=SECRET", nil)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != int64(len(body)) {
		t.Errorf("n=%d want %d", n, len(body))
	}
	assertExists(t, fs, TempArchiveName)
}

func TestDownloadFailsWithoutContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // chunked, no length
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	fs := newTestFs(t)
	if _, err := Download(context.Background(), fs, srv.URL, nil); err == nil {
		t.Fatal("expected error for missing content length")
	}
}

func TestDownloadFailsOnTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("short"))
		// Hijack and drop so the client sees EOF before 1000 bytes...
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	fs := newTestFs(t)
	if _, err := Download(context.Background(), fs, srv.URL, nil); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestDownloadRefusesRedirects(t *testing.T) {
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer dst.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusFound)
	}))
	defer srv.Close()

	fs := newTestFs(t)
	if _, err := Download(context.Background(), fs, srv.URL, nil); err == nil {
		t.Fatal("expected redirect refusal")
	}
}

func TestDownloadErrorsNeverLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	fs := newTestFs(t)
	_, err := Download(context.Background(), fs, srv.URL+"/x?Signature=TOPSECRET", nil)
	if err == nil {
		t.Fatal("expected 404 error")
	}
	if strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks URL: %v", err)
	}
}

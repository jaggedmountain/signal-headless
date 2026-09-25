// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package linkpreview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func pngBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func testServer(t *testing.T) (*httptest.Server, *Fetcher) {
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><html><head>
			<title>Fallback title</title>
			<meta property="og:title" content="The Hatter &amp; the Hare">
			<meta property="og:description" content="  Tea   at six,
			always. ">
			<meta property="og:image" content="/img.png">
			<meta property="article:published_time" content="2026-09-01T12:00:00Z">
			</head><body><meta property="og:title" content="ignored, in body"></body></html>`))
	})
	mux.HandleFunc("/plain", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Just a title</title></head></html>`))
	})
	mux.HandleFunc("/img.png", func(w http.ResponseWriter, r *http.Request) { w.Write(pngBytes()) })
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF"))
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &Fetcher{Dir: t.TempDir(), Transport: srv.Client().Transport, AllowPrivate: true}
}

func TestFetch(t *testing.T) {
	srv, f := testServer(t)
	p, err := f.Fetch(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "The Hatter & the Hare" || p.Description != "Tea at six, always." || p.Date != 1788264000000 {
		t.Fatalf("preview = %+v", p)
	}
	data, err := os.ReadFile(p.Image)
	if err != nil || !bytes.Equal(data, pngBytes()) || !strings.HasSuffix(p.Image, ".png") {
		t.Fatalf("image %s: %v", p.Image, err)
	}
	p, err = f.Fetch(context.Background(), srv.URL+"/plain")
	if err != nil || p.Title != "Just a title" || p.Image != "" {
		t.Fatalf("plain = %+v %v", p, err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/pdf"); err != ErrNoPreview {
		t.Fatalf("pdf: %v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/hop"); err == nil {
		t.Fatal("redirect to http must fail")
	}
}

func TestRefusesLocalTargets(t *testing.T) {
	f := &Fetcher{Dir: t.TempDir()}
	for _, u := range []string{
		"http://example.com/", // not https
		"https://127.0.0.1/", "https://[::1]/", "https://10.0.0.1/",
		"https://localhost/", "https://printer.local/", "https://router/", "https://nas.lan/",
		"https://user:pw@example.com/",
	} {
		pu, _ := url.Parse(u)
		if err := f.checkURL(pu); err == nil {
			t.Errorf("%s allowed", u)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.1.1", "100.64.0.1", "::1", "fe80::1", "fc00::1", "0.0.0.0"} {
		if err := f.checkAddr("tcp", net.JoinHostPort(ip, "443"), nil); err == nil {
			t.Errorf("dial to %s allowed", ip)
		}
	}
	if err := f.checkAddr("tcp", "93.184.215.14:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
}

func TestFirstURL(t *testing.T) {
	for in, want := range map[string]string{
		"see https://example.com/a?b=1.":       "https://example.com/a?b=1",
		"(https://example.com/x) and http://y": "https://example.com/x",
		"http://only-insecure.example/":        "",
		"no links":                             "",
	} {
		if got := FirstURL(in); got != want {
			t.Errorf("FirstURL(%q) = %q, want %q", in, got, want)
		}
	}
}

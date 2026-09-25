// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package linkpreview builds link previews for outgoing messages, the way
// Signal's apps do: the sender fetches the page's OpenGraph metadata and
// image, and the preview travels with the message (receivers never fetch).
//
// Fetching is deliberately narrow: https only, public addresses only
// (checked when connecting, so DNS can't point it at the local network),
// few redirects, a size cap on the page head and the image.
package linkpreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

type Preview struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Date        int64  `json:"date,omitempty"`  // ms
	Image       string `json:"image,omitempty"` // local file with the image
}

const (
	maxHTML      = 512 << 10
	maxImage     = 2 << 20
	maxRedirects = 3
	maxTitle     = 300
	maxDesc      = 500
)

var ErrNoPreview = errors.New("no preview available")

// Fetcher fetches previews; images are saved in Dir.
type Fetcher struct {
	Dir string
	// For tests: a custom transport, and permission to reach private
	// addresses (httptest servers listen on loopback).
	Transport    http.RoundTripper
	AllowPrivate bool

	client *http.Client
}

func (f *Fetcher) httpClient() *http.Client {
	if f.client != nil {
		return f.client
	}
	tr := f.Transport
	if tr == nil {
		d := &net.Dialer{Timeout: 5 * time.Second, Control: f.checkAddr}
		tr = &http.Transport{
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		}
	}
	f.client = &http.Client{
		Transport: tr,
		Timeout:   15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			return f.checkURL(req.URL)
		},
	}
	return f.client
}

// checkAddr runs for every connection, after DNS resolution.
func (f *Fetcher) checkAddr(network, address string, _ syscall.RawConn) error {
	if f.AllowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !publicIP(ip) {
		return fmt.Errorf("refusing to fetch previews from %s", host)
	}
	return nil
}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		// carrier-grade NAT 100.64.0.0/10
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
}

func (f *Fetcher) checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return errors.New("only https links get previews")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || u.User != nil {
		return errors.New("unsupported link")
	}
	if f.AllowPrivate {
		return nil
	}
	if net.ParseIP(host) != nil {
		return errors.New("no previews for IP-address links")
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return errors.New("no previews for local names")
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home.arpa", ".lan"} {
		if strings.HasSuffix(host, suffix) {
			return errors.New("no previews for local names")
		}
	}
	return nil
}

var urlRe = regexp.MustCompile(`https://[^\s<>"']+[^\s<>"'.,;:!?)\]}]`)

// FirstURL returns the first https link in text ("" if none).
func FirstURL(text string) string {
	return urlRe.FindString(text)
}

// Fetch builds a preview for rawURL.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Preview, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := f.checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; signal-headless link preview)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u.Host, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return nil, ErrNoPreview
	}
	meta := parseHead(io.LimitReader(resp.Body, maxHTML))
	p := &Preview{
		URL:         rawURL,
		Title:       clip(first(meta["og:title"], meta["twitter:title"], meta["title"]), maxTitle),
		Description: clip(first(meta["og:description"], meta["twitter:description"], meta["description"]), maxDesc),
	}
	if t, err := time.Parse(time.RFC3339, first(meta["article:published_time"], meta["og:published_time"])); err == nil {
		p.Date = t.UnixMilli()
	}
	if p.Title == "" {
		return nil, ErrNoPreview
	}
	if img := first(meta["og:image:secure_url"], meta["og:image"], meta["og:image:url"], meta["twitter:image"]); img != "" {
		if iu, err := resp.Request.URL.Parse(img); err == nil {
			if path, err := f.fetchImage(ctx, iu); err == nil {
				p.Image = path
			}
		}
	}
	return p, nil
}

func (f *Fetcher) fetchImage(ctx context.Context, u *url.URL) (string, error) {
	if err := f.checkURL(u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; signal-headless link preview)")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxImage {
		return "", errors.New("image too large")
	}
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}[http.DetectContentType(data)]
	if ext == "" {
		return "", errors.New("not a supported image")
	}
	sum := sha256.Sum256([]byte(u.String()))
	path := filepath.Join(f.Dir, "preview-"+hex.EncodeToString(sum[:8])+ext)
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// parseHead collects <title> and <meta> values from the document head.
func parseHead(r io.Reader) map[string]string {
	out := map[string]string{}
	z := html.NewTokenizer(r)
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				return out
			case "title":
				inTitle = true
			case "meta":
				var key, content string
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch strings.ToLower(string(k)) {
					case "property", "name":
						key = strings.ToLower(strings.TrimSpace(string(v)))
					case "content":
						content = string(v)
					}
				}
				if key != "" && content != "" {
					if _, seen := out[key]; !seen {
						out[key] = strings.TrimSpace(content)
					}
				}
			}
		case html.TextToken:
			if inTitle && out["title"] == "" {
				out["title"] = strings.TrimSpace(string(z.Text()))
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "title":
				inTitle = false
			case "head":
				return out
			}
		}
	}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

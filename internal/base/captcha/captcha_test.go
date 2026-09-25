package captcha

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeSiteVerify 记下收到的表单，按 reply 回应。
func fakeSiteVerify(t *testing.T, reply func(w http.ResponseWriter)) (*Client, *url.Values) {
	t.Helper()
	got := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("应当以表单 POST，得到 %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		*got = r.PostForm
		reply(w)
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, HTTP: &http.Client{Timeout: Timeout}}, got
}

func wantUnavailable(t *testing.T, err error) {
	t.Helper()
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != v1.CodeUnavailable {
		t.Fatalf("应当是 unavailable，得到 %v", err)
	}
}

func TestVerifyPasses(t *testing.T) {
	c, form := fakeSiteVerify(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"success":true,"hostname":"panel.example.com"}`)) })
	res, err := c.Verify(context.Background(), "the-secret", "the-token", "198.51.100.7")
	if err != nil || !res.Success {
		t.Fatalf("应当通过：%+v %v", res, err)
	}
	if form.Get("secret") != "the-secret" || form.Get("response") != "the-token" || form.Get("remoteip") != "198.51.100.7" {
		t.Fatalf("表单字段不对：%v", *form)
	}
}

func TestVerifyFailsWithCodes(t *testing.T) {
	c, _ := fakeSiteVerify(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response","timeout-or-duplicate"]}`))
	})
	res, err := c.Verify(context.Background(), "s", "t", "")
	if err != nil || res.Success || strings.Join(res.ErrorCodes, ",") != "invalid-input-response,timeout-or-duplicate" {
		t.Fatalf("应当不通过并带错误码：%+v %v", res, err)
	}
}

func TestVerifyOmitsEmptyRemoteIP(t *testing.T) {
	c, form := fakeSiteVerify(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"success":true}`)) })
	if _, err := c.Verify(context.Background(), "s", "t", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*form)["remoteip"]; ok {
		t.Fatalf("来源 IP 为空时不该带 remoteip：%v", *form)
	}
}

func TestVerifyUnavailable(t *testing.T) {
	t.Run("连不上", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		c := &Client{URL: "http://" + addr + "/siteverify", HTTP: &http.Client{Timeout: Timeout}}
		_, err = c.Verify(context.Background(), "s", "t", "")
		wantUnavailable(t, err)
	})
	t.Run("不是 2xx", func(t *testing.T) {
		c, _ := fakeSiteVerify(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) })
		_, err := c.Verify(context.Background(), "s", "t", "")
		wantUnavailable(t, err)
	})
	t.Run("回应不是 JSON", func(t *testing.T) {
		c, _ := fakeSiteVerify(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`<html>captive portal</html>`)) })
		_, err := c.Verify(context.Background(), "s", "t", "")
		wantUnavailable(t, err)
	})
	t.Run("JSON 里没有 success", func(t *testing.T) {
		c, _ := fakeSiteVerify(t, func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"hostname":"x"}`)) })
		_, err := c.Verify(context.Background(), "s", "t", "")
		wantUnavailable(t, err)
	})
	t.Run("超时", func(t *testing.T) {
		c, _ := fakeSiteVerify(t, func(w http.ResponseWriter) { time.Sleep(300 * time.Millisecond) })
		c.HTTP.Timeout = 50 * time.Millisecond
		_, err := c.Verify(context.Background(), "s", "t", "")
		wantUnavailable(t, err)
	})
}

func TestNewPointsAtCloudflare(t *testing.T) {
	c := New()
	if c.URL != SiteVerifyURL || c.HTTP.Timeout != 10*time.Second {
		t.Fatalf("默认客户端应当指向 Cloudflare、超时 10 秒：%+v", c)
	}
}

package freebox

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roganis/ez2pyro/internal/router"
	"github.com/roganis/ez2pyro/internal/telemetry"
)

var _ router.Router = (*Client)(nil)

// fakeBox mimics the parts of the Freebox OS API the client uses.
type fakeBox struct {
	channel  int
	polls    int
	sessions int
}

func (f *fakeBox) handler(t *testing.T) http.Handler {
	ok := func(w http.ResponseWriter, v any) {
		b, _ := json.Marshal(v)
		io.WriteString(w, `{"success":true,"result":`+string(b)+`}`)
	}
	const token = "apptoken123"
	mux := http.NewServeMux()
	mux.HandleFunc("/api_version", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"api_base_url":"/api/","api_version":"8.2","device_name":"Freebox Server"}`)
	})
	mux.HandleFunc("/api/v8/login/authorize/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ok(w, map[string]any{"app_token": token, "track_id": 7})
			return
		}
		f.polls++
		st := "pending"
		if f.polls > 1 {
			st = "granted"
		}
		ok(w, map[string]any{"status": st, "challenge": "c"})
	})
	mux.HandleFunc("/api/v8/login/", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"logged_in": false, "challenge": "chal"})
	})
	mux.HandleFunc("/api/v8/login/session/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		m := hmac.New(sha1.New, []byte(token))
		m.Write([]byte("chal"))
		if body["password"] != hex.EncodeToString(m.Sum(nil)) || body["app_id"] != AppID {
			io.WriteString(w, `{"success":false,"error_code":"invalid_token","msg":"bad"}`)
			return
		}
		f.sessions++
		ok(w, map[string]any{"session_token": "sess"})
	})
	mux.HandleFunc("/api/v8/login/logout/", func(w http.ResponseWriter, r *http.Request) { ok(w, nil) })
	mux.HandleFunc("/api/v8/wifi/ap/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fbx-App-Auth") != "sess" {
			io.WriteString(w, `{"success":false,"error_code":"auth_required","msg":"login"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stations/") {
			ok(w, []map[string]any{
				{"mac": "AA:BB:CC:00:00:01", "hostname": "steamdeck", "rx_rate": 20_000_000, "tx_rate": 100_000},
				{"mac": "AA:BB:CC:00:00:02", "hostname": "", "host": map[string]any{"primary_name": "Freebox Player"}, "rx_rate": 5_000_000, "tx_rate": 0},
			})
			return
		}
		ok(w, []map[string]any{{"id": 1, "name": "5G", "status": map[string]any{"state": "active", "primary_channel": f.channel},
			"config": map[string]any{"band": "5g", "channel_width": "80", "primary_channel": 0}}})
	})
	return mux
}

func TestFreeboxFlow(t *testing.T) {
	fb := &fakeBox{channel: 100}
	srv := httptest.NewServer(fb.handler(t))
	defer srv.Close()
	var out strings.Builder
	c := New(Options{URL: srv.URL, TokenFile: filepath.Join(t.TempDir(), "fb.json"),
		SelfMACs: []string{"aa:bb:cc:00:00:01"}, Out: &out, PairWait: 10 * time.Second})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "approve") {
		t.Fatalf("no pairing prompt: %q", out.String())
	}
	s, err := c.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := s[len(s)-1]
	if last.OtherMbps == nil || *last.OtherMbps != 40 || last.TopTalker != "Freebox Player" {
		t.Fatalf("other traffic %+v", last)
	}
	// Channel change → event; expired session → transparent re-login.
	fb.channel = 36
	c.session = "expired"
	s, err = c.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s[0].Event != telemetry.EventRouterChan {
		t.Fatalf("expected channel change event, got %+v", s[0])
	}
	if fb.sessions != 2 {
		t.Fatalf("sessions %d", fb.sessions)
	}
	c.Close()

	// A second client reuses the stored token without pairing again.
	polls := fb.polls
	c2 := New(Options{URL: srv.URL, TokenFile: c.opt.TokenFile, Out: io.Discard})
	if err := c2.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if fb.polls != polls {
		t.Fatal("paired again despite a stored token")
	}
}

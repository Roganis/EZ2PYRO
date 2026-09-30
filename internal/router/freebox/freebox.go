// Package freebox is a read-only client for the Freebox OS local HTTP API.
// It samples the box's Wi-Fi configuration and associated stations so the
// analysis can say things like "the box changed channel at 21:14:32" or
// "the Freebox Player pulled 40 Mbit/s during 9 of 11 stalls".
//
// Auth flow (Freebox OS API, "login" section): the app requests a token with
// POST login/authorize/, the user approves it on the box's front display,
// the token is stored in the user config folder, and each session is opened
// with POST login/session/ using password = HMAC-SHA1(app_token, challenge).
// Endpoint names follow the public developer docs (dev.freebox.fr); verify
// them against the current docs when the box firmware changes.
package freebox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/roganis/ez2pyro/internal/telemetry"
)

// AppID identifies linkdoctor to the box.
const AppID = "fr.ez2pyro.linkdoctor"

// DefaultURL is the box's well-known local address.
const DefaultURL = "http://mafreebox.freebox.fr"

// Options configures the client.
type Options struct {
	URL        string   // default DefaultURL
	TokenFile  string   // default <UserConfigDir>/linkdoctor/freebox.json
	SelfMACs   []string // our own Wi-Fi MACs, excluded from "other" traffic
	AppVersion string
	Out        io.Writer // where to print pairing instructions
	HTTP       *http.Client
	PairWait   time.Duration // how long to wait for approval (default 2 min)
}

// Client implements router.Router.
type Client struct {
	opt      Options
	apiBase  string // e.g. http://mafreebox.freebox.fr/api/v8/
	token    string
	session  string
	channels map[int]int // ap id → primary channel
}

// New creates a client.
func New(opt Options) *Client {
	if opt.URL == "" {
		opt.URL = DefaultURL
	}
	if opt.TokenFile == "" {
		if d, err := os.UserConfigDir(); err == nil {
			opt.TokenFile = filepath.Join(d, "linkdoctor", "freebox.json")
		}
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if opt.Out == nil {
		opt.Out = os.Stderr
	}
	if opt.PairWait == 0 {
		opt.PairWait = 2 * time.Minute
	}
	if opt.AppVersion == "" {
		opt.AppVersion = "1"
	}
	for i, m := range opt.SelfMACs {
		opt.SelfMACs[i] = strings.ToLower(m)
	}
	return &Client{opt: opt, channels: map[int]int{}}
}

type envelope struct {
	Success   bool            `json:"success"`
	Result    json.RawMessage `json:"result"`
	ErrorCode string          `json:"error_code"`
	Msg       string          `json:"msg"`
}

// APIError is an unsuccessful API response.
type APIError struct{ Code, Msg string }

func (e *APIError) Error() string { return "freebox: " + e.Code + ": " + e.Msg }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.session != "" {
		req.Header.Set("X-Fbx-App-Auth", c.session)
	}
	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("freebox %s: HTTP %d: %w", path, resp.StatusCode, err)
	}
	if !env.Success {
		return &APIError{env.ErrorCode, env.Msg}
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// discover reads /api_version to find the API base URL.
func (c *Client) discover(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.opt.URL, "/")+"/api_version", nil)
	if err != nil {
		return err
	}
	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("freebox not reachable at %s: %w", c.opt.URL, err)
	}
	defer resp.Body.Close()
	var v struct {
		APIBaseURL string `json:"api_base_url"`
		APIVersion string `json:"api_version"`
		DeviceName string `json:"device_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return fmt.Errorf("freebox api_version: %w", err)
	}
	major := strings.SplitN(v.APIVersion, ".", 2)[0]
	if _, err := strconv.Atoi(major); err != nil || v.APIBaseURL == "" {
		return fmt.Errorf("freebox api_version: unexpected %+v", v)
	}
	c.apiBase = strings.TrimRight(c.opt.URL, "/") + v.APIBaseURL + "v" + major + "/"
	return nil
}

type tokenFile struct {
	AppToken string `json:"app_token"`
	URL      string `json:"url"`
}

func (c *Client) loadToken() {
	b, err := os.ReadFile(c.opt.TokenFile)
	if err != nil {
		return
	}
	var t tokenFile
	if json.Unmarshal(b, &t) == nil && t.URL == c.opt.URL {
		c.token = t.AppToken
	}
}

func (c *Client) saveToken() error {
	if c.opt.TokenFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.opt.TokenFile), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(tokenFile{AppToken: c.token, URL: c.opt.URL})
	return os.WriteFile(c.opt.TokenFile, b, 0o600)
}

// pair requests an app token and waits for the user to approve it on the box.
func (c *Client) pair(ctx context.Context) error {
	host, _ := os.Hostname()
	var auth struct {
		AppToken string `json:"app_token"`
		TrackID  int    `json:"track_id"`
	}
	if err := c.do(ctx, http.MethodPost, "login/authorize/", map[string]string{
		"app_id": AppID, "app_name": "linkdoctor", "app_version": c.opt.AppVersion, "device_name": host,
	}, &auth); err != nil {
		return err
	}
	fmt.Fprintln(c.opt.Out, "Freebox: approve \"linkdoctor\" on the box's front display (use the arrow to select Yes)…")
	deadline := time.Now().Add(c.opt.PairWait)
	for time.Now().Before(deadline) {
		var st struct {
			Status string `json:"status"`
		}
		if err := c.do(ctx, http.MethodGet, "login/authorize/"+strconv.Itoa(auth.TrackID), nil, &st); err != nil {
			return err
		}
		switch st.Status {
		case "granted":
			c.token = auth.AppToken
			fmt.Fprintln(c.opt.Out, "Freebox: access granted; token saved to", c.opt.TokenFile)
			return c.saveToken()
		case "denied", "timeout", "unknown":
			return fmt.Errorf("freebox pairing %s", st.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("freebox pairing: no approval on the box in time")
}

func (c *Client) openSession(ctx context.Context) error {
	c.session = ""
	var l struct {
		Challenge string `json:"challenge"`
	}
	if err := c.do(ctx, http.MethodGet, "login/", nil, &l); err != nil {
		return err
	}
	mac := hmac.New(sha1.New, []byte(c.token))
	mac.Write([]byte(l.Challenge))
	var s struct {
		SessionToken string `json:"session_token"`
	}
	if err := c.do(ctx, http.MethodPost, "login/session/", map[string]string{
		"app_id": AppID, "password": hex.EncodeToString(mac.Sum(nil)),
	}, &s); err != nil {
		return err
	}
	c.session = s.SessionToken
	return nil
}

// Connect discovers the API, pairs if needed and opens a session.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	c.loadToken()
	if c.token == "" {
		if err := c.pair(ctx); err != nil {
			return err
		}
	}
	if err := c.openSession(ctx); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "invalid_token" {
			c.token = ""
			if err := c.pair(ctx); err != nil {
				return err
			}
			return c.openSession(ctx)
		}
		return err
	}
	return nil
}

// get performs an authenticated GET, reopening the session once if it expired.
func (c *Client) get(ctx context.Context, path string, out any) error {
	err := c.do(ctx, http.MethodGet, path, nil, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == "auth_required" {
		if err := c.openSession(ctx); err != nil {
			return err
		}
		return c.do(ctx, http.MethodGet, path, nil, out)
	}
	return err
}

type ap struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status struct {
		State            string `json:"state"`
		PrimaryChannel   int    `json:"primary_channel"`
		SecondaryChannel int    `json:"secondary_channel"`
		DFSCacState      string `json:"dfs_cac_state"`
	} `json:"status"`
	Config struct {
		Band           string `json:"band"`
		ChannelWidth   string `json:"channel_width"`
		PrimaryChannel int    `json:"primary_channel"`
	} `json:"config"`
}

type station struct {
	MAC      string `json:"mac"`
	Hostname string `json:"hostname"`
	RxRate   int64  `json:"rx_rate"` // bytes/s
	TxRate   int64  `json:"tx_rate"` // bytes/s
	Signal   int    `json:"signal"`
	Host     *struct {
		PrimaryName string `json:"primary_name"`
	} `json:"host"`
}

// Sample reads the access points and their stations.
func (c *Client) Sample(ctx context.Context) ([]telemetry.Sample, error) {
	var aps []ap
	if err := c.get(ctx, "wifi/ap/", &aps); err != nil {
		return nil, err
	}
	var out []telemetry.Sample
	var other float64
	talkers := map[string]float64{}
	for _, a := range aps {
		ch := a.Status.PrimaryChannel
		if prev, ok := c.channels[a.ID]; ok && ch != 0 && prev != ch {
			out = append(out, telemetry.Sample{Kind: telemetry.KindEvent, Event: telemetry.EventRouterChan,
				Channel: telemetry.Int(ch), Detail: fmt.Sprintf("box %s: channel %d → %d", a.Name, prev, ch)})
		}
		if ch != 0 {
			c.channels[a.ID] = ch
		}
		var sts []station
		if err := c.get(ctx, "wifi/ap/"+strconv.Itoa(a.ID)+"/stations/", &sts); err != nil {
			continue
		}
		for _, s := range sts {
			if c.self(s.MAC) {
				continue
			}
			mbps := float64(s.RxRate+s.TxRate) * 8 / 1e6
			other += mbps
			name := s.Hostname
			if s.Host != nil && s.Host.PrimaryName != "" {
				name = s.Host.PrimaryName
			}
			if name == "" {
				name = s.MAC
			}
			talkers[name] += mbps
		}
		band := strings.TrimSuffix(strings.TrimSuffix(a.Config.Band, "g"), "G")
		out = append(out, telemetry.Sample{Kind: telemetry.KindRouter, Band: normBand(band), Channel: telemetry.Int(ch),
			Detail: fmt.Sprintf("box %s %s ch %d %s, %d stations", a.Name, a.Config.Band, ch, a.Config.ChannelWidth, len(sts))})
	}
	top, topV := "", 0.0
	names := make([]string, 0, len(talkers))
	for n := range talkers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if talkers[n] > topV {
			top, topV = n, talkers[n]
		}
	}
	out = append(out, telemetry.Sample{Kind: telemetry.KindRouter, OtherMbps: telemetry.F64(other), TopTalker: top})
	return out, nil
}

func normBand(b string) string {
	switch {
	case strings.HasPrefix(b, "2"):
		return "2.4"
	case strings.HasPrefix(b, "5"):
		return "5"
	case strings.HasPrefix(b, "6"):
		return "6"
	}
	return b
}

func (c *Client) self(mac string) bool {
	mac = strings.ToLower(mac)
	for _, m := range c.opt.SelfMACs {
		if m == mac {
			return true
		}
	}
	return false
}

// Close logs out of the session.
func (c *Client) Close() error {
	if c.session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.do(ctx, http.MethodPost, "login/logout/", map[string]string{}, nil)
	c.session = ""
	return err
}

package doubao

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const origin = "https://www.doubao.com"
const botID = "7338286299411103781"

type object = map[string]any

func field(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
func array(v any) []any { a, _ := v.([]any); return a }
func textValue(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	default:
		return fmt.Sprint(v)
	}
}
func number(v any) int {
	// Persisted objects decoded by encoding/json use float64, whereas network
	// decoding uses json.Number. Epochs must work in both representations.
	if n, ok := v.(float64); ok {
		return int(n)
	}
	n, _ := strconv.Atoi(textValue(v))
	return n
}
func truthy(v any) bool {
	if v == nil || v == false || v == "" {
		return false
	}
	return true
}
func parsed(v any) any {
	if s, ok := v.(string); ok {
		var out any
		if decode(strings.NewReader(s), &out) == nil {
			return out
		}
	}
	return v
}
func decode(r io.Reader, out any) error { d := json.NewDecoder(r); d.UseNumber(); return d.Decode(out) }
func token() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}
func deviceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	n := binary.BigEndian.Uint64(b) & ((1 << 53) - 1)
	if n == 0 {
		n = 1
	}
	return strconv.FormatUint(n, 10)
}

type savedCookie struct {
	Source string      `json:"source"`
	Cookie http.Cookie `json:"cookie"`
}
type durableJar struct {
	base    http.CookieJar
	mu      sync.Mutex
	records map[string]savedCookie
}

func newJar() *durableJar {
	j, _ := cookiejar.New(nil)
	return &durableJar{base: j, records: map[string]savedCookie{}}
}
func (j *durableJar) Cookies(u *url.URL) []*http.Cookie { return j.base.Cookies(u) }
func (j *durableJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.base.SetCookies(u, cookies)
	for _, c := range cookies {
		copy := *c
		if copy.Path == "" {
			n := strings.LastIndex(u.Path, "/")
			copy.Path = "/"
			if n > 0 {
				copy.Path = u.Path[:n]
			}
		}
		if copy.MaxAge > 0 {
			copy.Expires = time.Now().Add(time.Duration(copy.MaxAge) * time.Second)
			copy.MaxAge = 0
		}
		domain := copy.Domain
		if domain == "" {
			domain = u.Hostname()
		}
		key := strings.TrimPrefix(domain, ".") + "|" + copy.Path + "|" + copy.Name
		if copy.MaxAge < 0 || !copy.Expires.IsZero() && copy.Expires.Before(time.Now()) {
			delete(j.records, key)
			continue
		}
		j.records[key] = savedCookie{Source: u.Scheme + "://" + u.Host + "/", Cookie: copy}
	}
}
func (j *durableJar) snapshot() []savedCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := []savedCookie{}
	for _, c := range j.records {
		out = append(out, c)
	}
	return out
}

type session struct {
	Version   int           `json:"version"`
	WebID     string        `json:"web_id"`
	DeviceID  string        `json:"device_id"`
	AccountID string        `json:"account_id,omitempty"`
	Cookies   []savedCookie `json:"cookies"`
}
type Client struct {
	dir       string
	session   session
	jar       *durableJar
	http      *http.Client
	mediaHTTP *http.Client
	mediaMu   sync.Mutex
	fileMu    sync.Mutex
	saveMu    sync.Mutex
	// deferToNewer stops a long-lived provider client from overwriting a
	// session.json that another process, normally helper login, replaced.
	deferToNewer bool
	saved        os.FileInfo
}

func newClient(dir string) (*Client, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	c := &Client{dir: dir, jar: newJar(), session: session{Version: 1, WebID: deviceID(), DeviceID: deviceID()}}
	path := filepath.Join(dir, "session.json")
	b, err := os.ReadFile(path)
	if err == nil {
		if err = decode(bytes.NewReader(b), &c.session); err != nil {
			return nil, errors.New("invalid saved session")
		}
		c.saved, _ = os.Stat(path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, row := range c.session.Cookies {
		u, e := trustedURL(row.Source)
		if e != nil {
			return nil, e
		}
		copy := row.Cookie
		c.jar.SetCookies(u, []*http.Cookie{&copy})
	}
	// HTTP/2 pings notice a connection that died without a reset; otherwise a
	// request on it waits for the task deadline.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	c.http = &http.Client{Transport: transport, Jar: c.jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}
func saveJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	name := path + "." + token() + ".tmp"
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(name)
	if _, err = file.Write(append(b, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func (c *Client) save() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	path := filepath.Join(c.dir, "session.json")
	if c.deferToNewer && c.replaced(path) {
		return nil
	}
	c.session.Cookies = c.jar.snapshot()
	if err := saveJSON(path, c.session); err != nil {
		return err
	}
	c.saved, _ = os.Stat(path)
	return nil
}

// replaced reports whether session.json is no longer the file this client
// last loaded or wrote. Callers hold saveMu.
func (c *Client) replaced(path string) bool {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return c.saved != nil
	case c.saved == nil:
		return true
	}
	return !os.SameFile(info, c.saved) || !info.ModTime().Equal(c.saved.ModTime()) || info.Size() != c.saved.Size()
}
func trustedURL(route string) (*url.URL, error) {
	base, _ := url.Parse(origin)
	ref, err := url.Parse(route)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	u := base.ResolveReference(ref)
	if u.Scheme != "https" || u.Port() != "" || u.User != nil || !(u.Hostname() == "www.doubao.com" || u.Hostname() == "doubao.com" || u.Hostname() == "passport.doubao.com") {
		return nil, errors.New("untrusted endpoint")
	}
	return u, nil
}
func (c *Client) request(ctx context.Context, method, route string, params url.Values, body any) (*http.Response, error) {
	u, err := trustedURL(route)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	defaults := map[string]string{"aid": "1044603", "device_platform": "web", "samantha_web": "1", "pc_version": "2.31.10", "doubao_pc_version": "2.31.10", "version_code": "20800", "language": "zh", "region": "CN", "sys_region": "CN", "web_id": c.session.WebID, "device_id": c.session.DeviceID}
	if u.Path == "/" || strings.HasPrefix(u.Path, "/passport/") {
		defaults["aid"] = "497858"
		delete(defaults, "pc_version")
		delete(defaults, "doubao_pc_version")
	}
	for k, v := range defaults {
		q.Set(k, v)
	}
	for k, vs := range params {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	var input io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), input)
	if err != nil {
		return nil, errors.New("request construction failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		if strings.HasPrefix(u.Path, "/alice/office/") || strings.HasPrefix(u.Path, "/alice/resource/") {
			req.Header.Set("Content-Type", "application/json")
		}
		if strings.HasPrefix(u.Path, "/im/") {
			req.Header.Set("Content-Type", "application/json; encoding=utf-8")
		}
	}
	if u.Path == "/alice/office/tool_local/chunk_stream" {
		req.Header.Set("Agw-Js-Conv", "str")
	}
	for _, cookie := range c.jar.Cookies(u) {
		if cookie.Name == "passport_csrf_token" {
			req.Header.Set("x-tt-passport-csrf-token", cookie.Value)
		}
	}
	r, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("upstream transport failed")
	}
	if err = c.save(); err != nil {
		r.Body.Close()
		return nil, errors.New("session persistence failed")
	}
	if r.StatusCode >= 400 {
		r.Body.Close()
		return nil, fmt.Errorf("upstream HTTP %d at %s", r.StatusCode, u.Path)
	}
	return r, nil
}
func (c *Client) json(ctx context.Context, method, route string, params url.Values, body any) (object, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	r, err := c.request(ctx, method, route, params, body)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	var value object
	if err := decode(io.LimitReader(r.Body, 8<<20), &value); err != nil {
		return nil, errors.New("upstream returned invalid JSON at " + route)
	}
	return value, nil
}
func (c *Client) account(ctx context.Context) error {
	v, err := c.json(ctx, "GET", "/passport/account/info/v2/", nil, nil)
	if err != nil {
		return err
	}
	d := field(v, "data")
	id := textValue(field(d, "user_id_str"))
	if id == "" {
		id = textValue(field(d, "user_id"))
	}
	if number(field(d, "error_code")) != 0 || id == "" || id == "0" || field(d, "is_visitor_account") == true {
		return errors.New("login_required")
	}
	if c.session.AccountID != "" && c.session.AccountID != id {
		return errors.New("account_changed")
	}
	if c.session.AccountID == "" {
		c.session.AccountID = id
	}
	return c.save()
}

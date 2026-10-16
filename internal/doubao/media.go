package doubao

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Independent ImageX/TOS implementation. Only prepare_upload uses the account
// cookie jar. Upload credentials never reach a redirect or the public evidence.
const mediaPartBytes = 5 << 20
const mediaMaxBytes = 8 << 20 // Adapter limit; the vendor may support larger files.

type mediaCredentials struct{ Access, Secret, Token string }

func mediaHash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func mediaMAC(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}
func mediaEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
func mediaQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		values := append([]string{}, q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, mediaEscape(k)+"="+mediaEscape(v))
		}
	}
	return strings.Join(parts, "&")
}
func mediaSign(method string, q url.Values, body []byte, credentials mediaCredentials, now time.Time) http.Header {
	stamp := now.UTC().Format("20060102T150405Z")
	h := http.Header{}
	h.Set("x-amz-date", stamp)
	h.Set("x-amz-security-token", credentials.Token)
	names := []string{"x-amz-date", "x-amz-security-token"}
	if body != nil {
		h.Set("x-amz-content-sha256", mediaHash(body))
		names = append(names, "x-amz-content-sha256")
	}
	sort.Strings(names)
	canonical := ""
	for _, k := range names {
		canonical += k + ":" + strings.Join(strings.Fields(h.Get(k)), " ") + "\n"
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{method, "/top/v1", mediaQuery(q), canonical, signed, mediaHash(body)}, "\n")
	scope := stamp[:8] + "/cn-north-1/imagex/aws4_request"
	value := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + mediaHash([]byte(request))
	key := mediaMAC([]byte("AWS4"+credentials.Secret), stamp[:8])
	for _, part := range []string{"cn-north-1", "imagex", "aws4_request"} {
		key = mediaMAC(key, part)
	}
	h.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%x", credentials.Access, scope, signed, mediaMAC(key, value)))
	return h
}

var mediaHTTP = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func mediaJSON(ctx context.Context, method, endpoint string, headers http.Header, body []byte) (object, error) {
	return mediaJSONWith(mediaHTTP, ctx, method, endpoint, headers, body)
}
func (c *Client) mediaJSON(ctx context.Context, method, endpoint string, headers http.Header, body []byte) (object, error) {
	client := c.mediaHTTP
	if client == nil {
		client = mediaHTTP
	}
	return mediaJSONWith(client, ctx, method, endpoint, headers, body)
}
func mediaJSONWith(client *http.Client, ctx context.Context, method, endpoint string, headers http.Header, body []byte) (object, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid media request")
	}
	req.Header = headers.Clone()
	r, err := client.Do(req)
	if err != nil {
		return nil, errors.New("media transport failed; mutation status may be unknown; no retry")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil, fmt.Errorf("media HTTP %d; redirects and automatic retries disabled", r.StatusCode)
	}
	var out object
	if err = decode(io.LimitReader(r.Body, 8<<20), &out); err != nil || out == nil {
		return nil, errors.New("invalid media JSON response")
	}
	return out, nil
}

func mediaOrigin(host string) (string, error) {
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Port() != "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid server-issued media origin")
	}
	return "https://" + u.Host, nil
}

func mediaVendor(out object, label string) (object, error) {
	if field(out, "ResponseMetadata", "Error") != nil {
		return nil, fmt.Errorf("ImageX %s rejected (%s)", label, textValue(field(out, "ResponseMetadata", "Error", "Code")))
	}
	result, ok := out["Result"].(map[string]any)
	if !ok {
		return nil, errors.New("missing ImageX result")
	}
	return result, nil
}

func mediaStorage(out object) (object, error) {
	data, ok := out["data"].(map[string]any)
	if number(out["code"]) != 2000 || !ok {
		return nil, fmt.Errorf("TOS rejected upload (%s)", textValue(out["code"]))
	}
	return data, nil
}

func (c *Client) uploadMedia(ctx context.Context, name string, data []byte, isImage bool, journal string) (object, object, error) {
	if len(data) == 0 || len(data) > mediaMaxBytes || filepath.Base(name) != name || strings.ContainsAny(name, "\r\n") {
		return nil, nil, errors.New("invalid media input; adapter limit is 8 MiB")
	}
	evidence := object{"name": name, "bytes": len(data), "sha256": mediaHash(data), "parts_confirmed": 0, "committed": false}
	state := object{"phase": "prepare_upload", "name": name, "sha256": mediaHash(data)}
	checkpoint := func(phase string) error { state["phase"] = phase; return saveJSON(journal, state) }
	if err := checkpoint("prepare_upload_dispatched"); err != nil {
		return nil, evidence, err
	}
	resourceType := 1
	if isImage {
		resourceType = 2
	}
	prepared, err := c.json(ctx, "POST", "/alice/resource/prepare_upload", nil, object{"tenant_id": "5", "scene_id": "5", "resource_type": resourceType})
	if err != nil {
		return nil, evidence, err
	}
	service := textValue(field(prepared, "data", "service_id"))
	credentials := mediaCredentials{textValue(field(prepared, "data", "upload_auth_token", "access_key")), textValue(field(prepared, "data", "upload_auth_token", "secret_key")), textValue(field(prepared, "data", "upload_auth_token", "session_token"))}
	if number(prepared["code"]) != 0 || service == "" || credentials.Access == "" || credentials.Secret == "" || credentials.Token == "" {
		return nil, evidence, errors.New("media authorization unavailable")
	}
	q := url.Values{"Action": {"ApplyImageUpload"}, "FileSize": {fmt.Sprint(len(data))}, "NeedFallback": {"true"}, "ServiceId": {service}, "Version": {"2018-08-01"}, "s": {token()[:12]}}
	if ext := filepath.Ext(name); ext != "" && len(ext) <= 8 {
		q.Set("FileExtension", ext)
	}
	if err = checkpoint("apply_dispatched"); err != nil {
		return nil, evidence, err
	}
	out, err := c.mediaJSON(ctx, "GET", "https://www.doubao.com/top/v1?"+mediaQuery(q), mediaSign("GET", q, nil, credentials, time.Now()), nil)
	if err != nil {
		return nil, evidence, err
	}
	result, err := mediaVendor(out, "apply")
	if err != nil {
		return nil, evidence, err
	}
	address, _ := result["UploadAddress"].(map[string]any)
	stores, hosts := array(address["StoreInfos"]), array(address["UploadHosts"])
	if len(stores) == 0 || len(hosts) == 0 {
		return nil, evidence, errors.New("missing upload address")
	}
	auth, store, sessionKey := textValue(field(stores[0], "Auth")), textValue(field(stores[0], "StoreUri")), textValue(address["SessionKey"])
	if auth == "" || store == "" || sessionKey == "" || strings.ContainsAny(auth, "\r\n") {
		return nil, evidence, errors.New("incomplete upload session")
	}
	storage, err := mediaOrigin(textValue(hosts[0]))
	if err != nil {
		return nil, evidence, err
	}
	headers := http.Header{}
	if extras, ok := address["UploadHeader"].(map[string]any); ok {
		for k, v := range extras {
			lower, text := strings.ToLower(k), textValue(v)
			if lower == "authorization" || lower == "cookie" || lower == "host" || lower == "x-storage-u" || strings.ContainsAny(k+text, "\r\n") {
				continue
			}
			headers.Set(k, text)
		}
	}
	headers.Set("Authorization", auth)
	headers.Set("X-Storage-U", mediaEscape(c.session.AccountID))
	base := storage + "/upload/v1/" + mediaEscape(store)
	state["storage_origin"], state["store_uri"] = storage, store
	if err = checkpoint("init_dispatched"); err != nil {
		return nil, evidence, err
	}
	out, err = c.mediaJSON(ctx, "POST", base+"?uploadmode=part&phase=init", headers, nil)
	if err != nil {
		return nil, evidence, err
	}
	init, err := mediaStorage(out)
	if err != nil {
		return nil, evidence, err
	}
	uploadID := textValue(init["uploadid"])
	if uploadID == "" {
		return nil, evidence, errors.New("missing multipart ID")
	}
	state["upload_id"] = uploadID
	parts := []string{}
	for offset := 0; offset < len(data); offset += mediaPartBytes {
		end := offset + mediaPartBytes
		if end > len(data) {
			end = len(data)
		}
		part := data[offset:end]
		n, checksum := len(parts)+1, fmt.Sprintf("%08x", crc32.ChecksumIEEE(part))
		headers.Set("Content-CRC32", checksum)
		if err = checkpoint(fmt.Sprintf("part_%d_dispatched", n)); err != nil {
			return nil, evidence, err
		}
		out, err = c.mediaJSON(ctx, "POST", base+"?"+mediaQuery(url.Values{"uploadid": {uploadID}, "part_number": {fmt.Sprint(n)}, "phase": {"transfer"}}), headers, part)
		if err != nil {
			return nil, evidence, err
		}
		if _, err = mediaStorage(out); err != nil {
			return nil, evidence, err
		}
		parts = append(parts, fmt.Sprintf("%d:%s", n, checksum))
		evidence["parts_confirmed"] = len(parts)
	}
	headers.Del("Content-CRC32")
	if err = checkpoint("finish_dispatched"); err != nil {
		return nil, evidence, err
	}
	out, err = c.mediaJSON(ctx, "POST", base+"?"+mediaQuery(url.Values{"uploadmode": {"part"}, "phase": {"finish"}, "uploadid": {uploadID}}), headers, []byte(strings.Join(parts, ",")))
	if err != nil {
		return nil, evidence, err
	}
	finish, err := mediaStorage(out)
	if err != nil {
		return nil, evidence, err
	}
	if textValue(finish["key"]) == "" {
		return nil, evidence, errors.New("unconfirmed storage object")
	}
	state["storage_key"] = finish["key"]
	q = url.Values{"Action": {"CommitImageUpload"}, "ServiceId": {service}, "Version": {"2018-08-01"}}
	body := []byte(jsonText(object{"SessionKey": sessionKey}))
	headers = mediaSign("POST", q, body, credentials, time.Now())
	headers.Set("Content-Type", "application/json")
	if err = checkpoint("commit_dispatched"); err != nil {
		return nil, evidence, err
	}
	out, err = c.mediaJSON(ctx, "POST", "https://www.doubao.com/top/v1?"+mediaQuery(q), headers, body)
	if err != nil {
		return nil, evidence, err
	}
	result, err = mediaVendor(out, "commit")
	if err != nil {
		return nil, evidence, err
	}
	committed := array(result["Results"])
	if len(committed) == 0 || textValue(field(committed[0], "Uri")) == "" {
		return nil, evidence, errors.New("missing committed media URI")
	}
	uri := textValue(field(committed[0], "Uri"))
	state["uri"] = uri
	evidence["committed"] = true
	record := object{"type": 3, "identifier": token(), "parse_state": 0, "review_state": 0, "upload_status": 1, "progress": 100, "src": ""}
	if isImage {
		record["type"] = 1
		record["image"] = object{"name": name, "uri": uri, "image_ori": object{"url": "", "width": 0, "height": 0, "format": strings.TrimPrefix(filepath.Ext(name), "."), "url_formats": object{}}}
	} else {
		record["file"] = object{"uri": uri, "url": "", "file_type": 0, "name": name, "size": len(data)}
	}
	state["record"] = record
	if err = checkpoint("committed"); err != nil {
		return nil, evidence, err
	}
	return record, evidence, nil
}

func appendMedia(body object, records []any) {
	message := array(body["messages"])[0].(map[string]any)
	message["content_block"] = append(array(message["content_block"]), object{"block_type": 10052, "content": object{"attachment_block": object{"attachments": records}, "pc_event_block": ""}, "block_id": token(), "parent_id": "", "meta_info": []any{}, "append_fields": []any{}})
}

package doubao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var fileIDPattern = regexp.MustCompile(`^file_[a-f0-9]{32}$`)
var inputMIMEs = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif", ".pdf": "application/pdf", ".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv", ".json": "application/json", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation"}

func (s *Service) StoreFile(name string, data []byte) (string, error) {
	return s.c.storeFile(name, data)
}
func (c *Client) storeFile(name string, data []byte) (string, error) {
	if filepath.Base(name) != name || name == "" || len(data) == 0 || len(data) > mediaMaxBytes || inputMIMEs[strings.ToLower(filepath.Ext(name))] == "" {
		return "", apiFail(400, "invalid_file", "Unsupported file name/type or file exceeds 8 MiB")
	}
	c.fileMu.Lock()
	defer c.fileMu.Unlock()
	lock, err := lockFile(c.dir, "files.lock", false)
	if err != nil {
		return "", err
	}
	defer unlockFile(lock)
	files, err := c.listFilesLocked(time.Now())
	if err != nil {
		return "", err
	}
	var total int64
	for _, file := range files {
		total += file.Bytes
	}
	if len(files) >= maxStoredFiles || total+int64(len(data)) > maxStoredBytes {
		return "", apiFail(429, "file_capacity", "File registry capacity reached; delete files before uploading more")
	}
	dir := filepath.Join(c.dir, "files")
	id := "file_" + token()
	if err := os.WriteFile(filepath.Join(dir, id+".blob"), data, 0600); err != nil {
		return "", err
	}
	if err := saveJSON(filepath.Join(dir, id+".json"), object{"name": name, "expires": time.Now().Add(7 * 24 * time.Hour).Unix(), "sha256": mediaHash(data)}); err != nil {
		_ = os.Remove(filepath.Join(dir, id+".blob"))
		return "", err
	}
	return id, nil
}
func (c *Client) loadFile(id string) (string, []byte, error) {
	c.fileMu.Lock()
	defer c.fileMu.Unlock()
	lock, err := lockFile(c.dir, "files.lock", false)
	if err != nil {
		return "", nil, err
	}
	defer unlockFile(lock)
	if !fileIDPattern.MatchString(id) {
		return "", nil, apiFail(400, "invalid_file_id", "Invalid file_id")
	}
	data, err := os.ReadFile(filepath.Join(c.dir, "files", id+".json"))
	if err != nil {
		return "", nil, apiFail(404, "file_not_found", "Unknown file_id for this account")
	}
	var info object
	if json.Unmarshal(data, &info) != nil {
		return "", nil, errors.New("invalid file record")
	}
	if int64(number(info["expires"])) < time.Now().Unix() {
		return "", nil, apiFail(410, "file_expired", "File has expired")
	}
	blob, err := os.ReadFile(filepath.Join(c.dir, "files", id+".blob"))
	if err != nil {
		return "", nil, err
	}
	if mediaHash(blob) != info["sha256"] {
		return "", nil, errors.New("stored file checksum mismatch")
	}
	return textValue(info["name"]), blob, nil
}

func (c *Client) readMediaSource(ctx context.Context, source, name string) (string, string, []byte, error) {
	var data []byte
	autoName := name == ""
	kind := ""
	var err error
	if strings.HasPrefix(source, "data:") {
		header, encoded, ok := strings.Cut(source, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return "", "", nil, apiFail(400, "invalid_media", "Expected base64 data URL")
		}
		kind = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		data, err = base64.StdEncoding.DecodeString(encoded)
	} else if strings.HasPrefix(source, "https://") {
		u, parseErr := url.Parse(source)
		if parseErr != nil || u.User != nil || u.Port() != "" && u.Port() != "443" {
			return "", "", nil, apiFail(400, "invalid_media_url", "Expected a public HTTPS URL")
		}
		hosts, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
		if lookupErr != nil || len(hosts) == 0 {
			return "", "", nil, errors.New("media URL lookup failed")
		}
		for _, host := range hosts {
			ip := host.IP
			if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return "", "", nil, apiFail(400, "invalid_media_url", "Media URL must resolve to public addresses")
			}
		}
		req, _ := http.NewRequestWithContext(context.WithValue(ctx, publicMediaContextKey{}, true), "GET", source, nil)
		client := c.mediaHTTP
		if client == nil {
			var cleanup func()
			client, cleanup, err = publicDownloadClient(req, "direct://")
			if err != nil {
				return "", "", nil, err
			}
			defer cleanup()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		response, fetchErr := client.Do(req)
		if fetchErr != nil {
			return "", "", nil, errors.New("media URL download failed")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return "", "", nil, apiFail(400, "media_download_failed", "Media URL must return 200 without redirects")
		}
		data, err = io.ReadAll(io.LimitReader(response.Body, mediaMaxBytes+1))
		kind, _, _ = mime.ParseMediaType(response.Header.Get("Content-Type"))
		if name == "" {
			name = filepath.Base(u.Path)
		}
	} else {
		if name == "" {
			return "", "", nil, apiFail(400, "invalid_media", "File needs a filename and inline bytes")
		}
		data, err = base64.StdEncoding.DecodeString(source)
	}
	if err != nil || len(data) == 0 || len(data) > mediaMaxBytes {
		return "", "", nil, apiFail(400, "invalid_media", "Invalid base64 or media exceeds 8 MiB")
	}
	if kind == "" {
		kind = inputMIMEs[strings.ToLower(filepath.Ext(name))]
	}
	if name == "" || name == "." || name == "/" || autoName && inputMIMEs[strings.ToLower(filepath.Ext(name))] == "" {
		for ext, value := range inputMIMEs {
			if value == kind {
				name = "attachment" + ext
				break
			}
		}
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, "\r\n") || inputMIMEs[strings.ToLower(filepath.Ext(name))] == "" {
		return "", "", nil, apiFail(400, "unsupported_file_type", "Unsupported media type or filename")
	}
	return name, kind, data, nil
}
func (c *Client) resolveMediaPart(ctx context.Context, part any) (string, string, []byte, error) {
	switch field(part, "type") {
	case "image_url":
		return c.readMediaSource(ctx, textValue(field(part, "image_url", "url")), "")
	case "file":
		if id := textValue(field(part, "file", "file_id")); id != "" {
			name, data, err := c.loadFile(id)
			return name, inputMIMEs[strings.ToLower(filepath.Ext(name))], data, err
		}
		source := textValue(field(part, "file", "file_data"))
		if source == "" {
			source = textValue(field(part, "file", "file_url"))
		}
		return c.readMediaSource(ctx, source, textValue(field(part, "file", "filename")))
	}
	return "", "", nil, apiFail(400, "unsupported_content", "Unsupported media part")
}
func (c *Client) cachedMedia(ctx context.Context, name string, data []byte, isImage bool) (object, error) {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	cache := filepath.Join(c.dir, "uploads")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, err
	}
	key := mediaHash(data) + fmt.Sprint(isImage)
	path := filepath.Join(cache, key+".json")
	entries, _ := filepath.Glob(filepath.Join(cache, "*.json"))
	for _, entryPath := range entries {
		if strings.HasSuffix(entryPath, "-journal.json") {
			continue
		}
		if b, err := os.ReadFile(entryPath); err == nil {
			var record object
			if json.Unmarshal(b, &record) == nil && int64(number(record["expires"])) <= time.Now().Unix() {
				_ = os.Remove(entryPath)
				_ = os.Remove(strings.TrimSuffix(entryPath, ".json") + "-journal.json")
			}
		}
	}

	if b, err := os.ReadFile(path); err == nil {
		var entry object
		if json.Unmarshal(b, &entry) == nil && int64(number(entry["expires"])) > time.Now().Unix() {
			if record, ok := entry["record"].(map[string]any); ok {
				return record, nil
			}
		}
	}
	journal := filepath.Join(cache, key+"-journal.json")
	if b, readErr := os.ReadFile(journal); readErr == nil {
		var saved object
		if json.Unmarshal(b, &saved) == nil && saved["phase"] == "committed" {
			if record, ok := saved["record"].(map[string]any); ok {
				if err := saveJSON(path, object{"expires": time.Now().Add(6 * time.Hour).Unix(), "record": record}); err != nil {
					return nil, err
				}
				return record, nil
			}
		}
		return nil, apiFail(409, "media_upload_unconfirmed", "Existing media upload needs reconciliation; upload was not repeated")
	}
	entries, _ = filepath.Glob(filepath.Join(cache, "*-journal.json"))
	if len(entries) >= 256 {
		return nil, apiFail(429, "upload_cache_capacity", "Upload cache capacity reached; inspect pending uploads before adding files")
	}
	record, _, err := c.uploadMedia(ctx, name, data, isImage, journal)
	if err != nil {
		return nil, err
	}
	if err = saveJSON(path, object{"expires": time.Now().Add(6 * time.Hour).Unix(), "record": record}); err != nil {
		return nil, err
	}
	return record, nil
}
func (c *Client) prepareAPIMedia(ctx context.Context, req apiRequest) (apiRequest, []any, error) {
	clone := req
	clone.Messages = append([]apiMessage{}, req.Messages...)
	records := []any{}
	for i, m := range clone.Messages {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		text := []string{}
		for _, part := range parts {
			if field(part, "type") == "text" {
				text = append(text, textValue(field(part, "text")))
				continue
			}
			if len(records) >= 12 {
				return req, nil, apiFail(400, "too_many_attachments", "At most 12 attachments per request")
			}
			name, kind, data, err := c.resolveMediaPart(ctx, part)
			if err != nil {
				return req, nil, err
			}
			isImage := strings.HasPrefix(kind, "image/")
			record, err := c.cachedMedia(ctx, name, data, isImage)
			if err != nil {
				return req, nil, err
			}
			records = append(records, record)
			text = append(text, fmt.Sprintf("[附件 %d：%s，来自第 %d 条 %s 消息]", len(records), name, i+1, m.Role))
		}
		clone.Messages[i].Content = strings.Join(text, "\n")
	}
	return clone, records, nil
}

func apiResultText(content any) string {
	if parts, ok := content.([]any); ok {
		return jsonText(parts)
	}
	return textValue(content)
}
func (c *Client) nativeResultContent(ctx context.Context, value string) ([]any, error) {
	parts, ok := parsed(value).([]any)
	recognized := ok && len(parts) > 0
	for _, part := range parts {
		kind := field(part, "type")
		if kind != "text" && kind != "image_url" && kind != "file" {
			recognized = false
		}
	}
	if !recognized {
		return []any{object{"type": "text", "text": value}}, nil
	}
	if scope, ok := ctx.Value(toolMediaScopeKey{}).(toolMediaScope); ok && scope.model.ID != "" {
		hasMedia := false
		for _, part := range parts {
			if field(part, "type") == "image_url" || field(part, "type") == "file" {
				hasMedia = true
			}
		}
		if hasMedia {
			observation, err := c.observeToolMedia(ctx, scope, parts)
			if err != nil {
				return nil, err
			}
			return []any{object{"type": "text", "text": "Adapter media reader observations (from the real tool attachments):\n" + observation}}, nil
		}
	}
	result := []any{}
	for _, part := range parts {
		if field(part, "type") == "text" {
			result = append(result, part)
			continue
		}
		name, kind, data, err := c.resolveMediaPart(ctx, part)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(kind, "image/") {
			result = append(result, object{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": kind})
		} else {
			result = append(result, object{"type": "resource", "resource": object{"uri": "file:///" + url.PathEscape(name), "mimeType": kind, "blob": base64.StdEncoding.EncodeToString(data)}})
		}
	}
	return result, nil
}

func (c *Client) nativeToolReply(ctx context.Context, native object, value string, batch bool) (object, error) {
	content := []any{}
	if batch {
		rows := array(field(parsed(value), "results"))
		media := false
		for _, row := range rows {
			for _, part := range array(parsed(field(row, "result"))) {
				if field(part, "type") == "image_url" || field(part, "type") == "file" {
					media = true
				}
			}
		}
		if !media {
			content = []any{object{"type": "text", "text": value}}
		} else {
			for _, row := range rows {
				content = append(content, object{"type": "text", "text": fmt.Sprintf("Function %s, batch index %v:", textValue(field(row, "name")), field(row, "index"))})
				parts, err := c.nativeResultContent(ctx, textValue(field(row, "result")))
				if err != nil {
					return nil, err
				}
				content = append(content, parts...)
			}
		}
	} else {
		var err error
		content, err = c.nativeResultContent(ctx, value)
		if err != nil {
			return nil, err
		}
	}
	return object{"id": native["id"], "name": native["name"], "tool_input": native["input"], "raw_result": jsonText(object{"content": content, "isError": false})}, nil
}

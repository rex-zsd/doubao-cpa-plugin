package doubao

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileRegistryIsolationExpirationAndDelete(t *testing.T) {
	c, _ := newClient(t.TempDir())
	other, _ := newClient(t.TempDir())
	id, err := c.storeFile("sample.pdf", []byte("%PDF-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	name, data, err := c.loadFile(id)
	if err != nil || name != "sample.pdf" || string(data) != "%PDF-fixture" {
		t.Fatal(name, err)
	}
	if _, _, err = other.loadFile(id); err == nil {
		t.Fatal("file crossed accounts")
	}
	files, err := c.listFiles()
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if err = c.deleteFile(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.loadFile(id); err == nil {
		t.Fatal("deleted file readable")
	}
	id, err = c.storeFile("other.txt", []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	_ = saveJSON(filepath.Join(c.dir, "files", id+".json"), object{"name": "other.txt", "expires": time.Now().Add(-time.Second).Unix()})
	files, err = c.listFiles()
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
	if _, err = os.Stat(filepath.Join(c.dir, "files", id+".blob")); !os.IsNotExist(err) {
		t.Fatal("expired blob retained")
	}
	if _, _, err = c.loadFile("../../session.json"); err == nil {
		t.Fatal("file path accepted")
	}
}
func TestMediaToolContentPreservesOrdinaryJSONArrays(t *testing.T) {
	c, _ := newClient(t.TempDir())
	ctx := context.Background()
	for _, text := range []string{`[1,2,3]`, `[{"value":"a"}]`, `[]`} {
		parts, err := c.nativeResultContent(ctx, text)
		if err != nil || len(parts) != 1 || field(parts[0], "type") != "text" || field(parts[0], "text") != text {
			t.Fatal(text, parts, err)
		}
	}
	id, err := c.storeFile("data.txt", []byte("a random file value"))
	if err != nil {
		t.Fatal(err)
	}
	value := jsonText([]any{object{"type": "text", "text": "result"}, object{"type": "image_url", "image_url": object{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNG"))}}, object{"type": "file", "file": object{"file_id": id}}})
	parts, err := c.nativeResultContent(ctx, value)
	if err != nil || len(parts) != 3 {
		t.Fatal(parts, err)
	}
	if field(parts[1], "type") != "image" || field(parts[2], "resource", "blob") != base64.StdEncoding.EncodeToString([]byte("a random file value")) {
		t.Fatal("media not forwarded", parts)
	}
}
func TestCachedMediaUncertainWriteNeverResubmits(t *testing.T) {
	c, _ := newClient(t.TempDir())
	data := []byte("unknown upload")
	key := mediaHash(data) + "false"
	dir := filepath.Join(c.dir, "uploads")
	_ = os.Mkdir(dir, 0700)
	_ = saveJSON(filepath.Join(dir, key+"-journal.json"), object{"phase": "commit_dispatched"})
	if _, err := c.cachedMedia(context.Background(), "data.txt", data, false); err == nil || !strings.Contains(err.Error(), "not repeated") {
		t.Fatal("unconfirmed upload repeated", err)
	}
}
func TestMediaRejectsNonPublicURLs(t *testing.T) {
	c, _ := newClient(t.TempDir())
	for _, url := range []string{"https://127.0.0.1/a.png", "https://169.254.169.254/a.png", "https://[::1]/a.png", "https://user:pass@example.com/a.png"} {
		if _, _, _, err := c.readMediaSource(context.Background(), url, ""); err == nil {
			t.Fatal(url)
		}
	}
}

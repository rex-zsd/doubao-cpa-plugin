package doubao

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxStoredFiles = 128
const maxStoredBytes = 256 << 20

type FileInfo struct {
	ID      string `json:"id"`
	Name    string `json:"filename"`
	Bytes   int64  `json:"bytes"`
	Expires int64  `json:"expires_at"`
}

func (c *Client) listFilesLocked(now time.Time) ([]FileInfo, error) {
	dir := filepath.Join(c.dir, "files")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "file_*.json"))
	if err != nil {
		return nil, err
	}
	files := []FileInfo{}
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if !fileIDPattern.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record object
		if json.Unmarshal(data, &record) != nil {
			return nil, errors.New("invalid file registry record")
		}
		if int64(number(record["expires"])) <= now.Unix() {
			if err = os.Remove(filepath.Join(dir, id+".blob")); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if err = os.Remove(path); err != nil {
				return nil, err
			}
			continue
		}
		info, err := os.Stat(filepath.Join(dir, id+".blob"))
		if err != nil {
			return nil, err
		}
		files = append(files, FileInfo{ID: id, Name: textValue(record["name"]), Bytes: info.Size(), Expires: int64(number(record["expires"]))})
	}
	// A crash between blob and metadata writes can leave an unreferenced blob.
	blobs, _ := filepath.Glob(filepath.Join(dir, "file_*.blob"))
	for _, blob := range blobs {
		if _, err := os.Stat(strings.TrimSuffix(blob, ".blob") + ".json"); os.IsNotExist(err) {
			_ = os.Remove(blob)
		}
	}
	return files, nil
}
func (c *Client) listFiles() ([]FileInfo, error) {
	c.fileMu.Lock()
	defer c.fileMu.Unlock()
	lock, err := lockFile(c.dir, "files.lock", false)
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)
	return c.listFilesLocked(time.Now())
}
func (c *Client) deleteFile(id string) error {
	if !fileIDPattern.MatchString(id) {
		return apiFail(400, "invalid_file_id", "Invalid file_id")
	}
	c.fileMu.Lock()
	defer c.fileMu.Unlock()
	lock, err := lockFile(c.dir, "files.lock", false)
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	dir := filepath.Join(c.dir, "files")
	if _, err := os.Stat(filepath.Join(dir, id+".json")); os.IsNotExist(err) {
		return apiFail(404, "file_not_found", "Unknown file_id for this account")
	}
	if err := os.Remove(filepath.Join(dir, id+".json")); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, id+".blob")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

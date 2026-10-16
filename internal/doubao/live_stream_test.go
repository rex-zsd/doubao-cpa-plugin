package doubao

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in diagnostic: records only event structure, never model reasoning.
func TestLiveStreamShape(t *testing.T) {
	dir := os.Getenv("DOUBAO_LIVE_STATE_DIR")
	if dir == "" {
		t.Skip("live account not configured")
	}
	c, err := newClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	models, err := c.models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	model, _, err := selectModel(models, "豆包 2.1 Pro", "work")
	if err != nil {
		t.Fatal(err)
	}
	shapes := []any{}
	state := &nativeVisibleStream{}
	updates := 0
	last := ""
	var shape func(any, string) any
	shape = func(v any, key string) any {
		switch a := v.(type) {
		case map[string]any:
			r := object{}
			for k, b := range a {
				r[k] = shape(b, k)
			}
			return r
		case []any:
			r := []any{}
			for _, b := range a {
				r = append(r, shape(b, key))
			}
			return r
		case string:
			if key == "patch_op" || strings.Contains(key, "path") || key == "op" || key == "patch_type" {
				return a
			}
			return object{"string_bytes": len(a)}
		default:
			return v
		}
	}
	receipt, err := c.runObserved(ctx, buildBody("请直接写一篇约600字的中文介绍，解释如何种植番茄，只写正文，不要调用工具。", "", "work", model), filepath.Join(dir, "stream-shape-receipt.json"), func(name string, data object) error {
		text := state.observe(name, data)
		if len(text) > len(last) && strings.HasPrefix(text, last) {
			updates++
			last = text
		}
		if len(shapes) < 2000 {
			shapes = append(shapes, object{"event": name, "data": shape(data, "")})
		}
		return nil
	})
	if saveErr := saveJSON(filepath.Join(dir, "stream-shapes.json"), shapes); saveErr != nil {
		t.Fatal(saveErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Completed {
		t.Fatal("no final reply")
	}
	if last == "" || !strings.HasPrefix(receipt.Reply, last) || updates < 2 {
		t.Fatalf("incremental prefix failed: updates=%d prefix_bytes=%d final_bytes=%d", updates, len(last), len(receipt.Reply))
	}
	if err = saveJSON(filepath.Join(dir, "stream-proof.json"), object{"success": true, "incremental_updates": updates, "visible_bytes": len(last), "final_bytes": len(receipt.Reply), "verified_prefix": true}); err != nil {
		t.Fatal(err)
	}
}

package doubao

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Contract captured from the official 2.31.3 serializer and a live Sol catalog.
// Keep an unknown catalog property too: selecting a model must not silently
// discard a new server-supplied option.
func nativeCatalogParams() object {
	return object{"provider_id": "cis", "model_type": "custom", "memory_profile": "1000k_80", "total_window_size": "1000000", "disable_cot_summary": "false", "future_option": "preserve"}
}

func TestCatalogRetainsNativeModelConfiguration(t *testing.T) {
	c, err := newClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry := object{"model_item_key": "5262373378", "item_id": 10001, "name": "GPT-6.1 Sol", "model_extra_params": nativeCatalogParams(), "reasoning_effort_config": object{"option_list": []any{object{"level": 7}, object{"level": 3}}}}
	response := object{"code": 0, "data": object{"entry_list": []any{object{"action_bar_key": "coco_deep_thinking", "active_switch_conf": object{"menu_conf_v2": object{"mode_list": object{"item_list": []any{object{"agent_mode": 1, "model_list": object{"item_list": []any{entry}}}}}}}}}}}
	c.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/alice/slot/action_bar_v3/brief_list" {
			t.Fatal(req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(jsonText(response)))}, nil
	})
	models, err := c.models(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatal(models, err)
	}
	m := models[0]
	if !reflect.DeepEqual(m.ExtraParams, nativeCatalogParams()) || !reflect.DeepEqual(m.Efforts, []int{3, 7}) || m.Window != 1000000 || m.Provider != "cis" || m.Think != 10001 {
		t.Fatal("catalog model configuration was lost", m)
	}
}

func TestNativeModelConfigurationForNewFollowupAndToolTurns(t *testing.T) {
	m := Model{ID: "5262373378", Think: 10001, Provider: "cis", ExtraParams: nativeCatalogParams(), Efforts: []int{3, 4, 5, 6, 7}}
	for _, conversation := range []string{"", "1000000000001"} {
		for _, tools := range []bool{false, true} {
			for _, effort := range []int{0, 2, 3, 6, 9} {
				body := buildBody("fixture", conversation, "work", m)
				if tools {
					body = localToolBody("fixture", conversation, m, "/owned", "sandbox", "123", "456", []any{})
				}
				applyEffort(body, m, effort)
				config := field(body, "option", "model_config")
				if !reflect.DeepEqual(field(config, "model_extra_params"), nativeCatalogParams()) || field(config, "model_item_key") != m.ID {
					t.Fatal("model metadata was not serialized", config)
				}
				if field(body, "ext", "reasoning_effort") != nil {
					t.Fatal("legacy ext reasoning override remains")
				}
				chosen, value := 0, ""
				if effort > 0 {
					chosen = min(max(effort, 3), 7)
					value = textValue(chosen)
					if field(config, "reasoning_effort") != chosen {
						t.Fatal("native model override must be numeric", config)
					}
				} else if field(config, "reasoning_effort") != nil {
					t.Fatal("default reasoning was overridden")
				}
				aggregate := object{"conversation_mode": "", "mode_id": "3", "model_item_key": m.ID, "agent_mode": "1", "provider_id": "cis", "reasoning_effort": value}
				if !reflect.DeepEqual(field(body, "option", "aggregate_params"), aggregate) {
					t.Fatal("aggregate model selection differs from native contract")
				}
				init := field(body, "option", "conversation_init_ext")
				if conversation == "" {
					if field(init, "model_item_key") != m.ID || field(init, "mode_id") != "3" || effort > 0 && field(init, "reasoning_effort") != value {
						t.Fatal("new-conversation model settings missing", init)
					}
				} else if init != nil {
					t.Fatal("follow-up includes conversation initialization")
				}
				// Each request gets its own metadata map; a request change cannot
				// corrupt the catalog shared by concurrent turns.
				field(config, "model_extra_params").(map[string]any)["memory_profile"] = "request-only"
				if m.ExtraParams["memory_profile"] != "1000k_80" {
					t.Fatal("request mutated the shared catalog")
				}
			}
		}
	}
}

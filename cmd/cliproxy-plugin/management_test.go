package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// hostManagementResponse mirrors CPA's pluginapi.ManagementResponse decoding.
type hostManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func callManagement(t *testing.T, method, path string, query url.Values, body string) hostManagementResponse {
	t.Helper()
	raw, _ := json.Marshal(object{"Method": method, "Path": path, "Headers": http.Header{}, "Query": query, "Body": []byte(body), "host_callback_id": "cb"})
	result, err := dispatch("management.handle", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var response hostManagementResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestManagementRegistrationDeclaresPageAndRoutes(t *testing.T) {
	result, err := dispatch("management.register", []byte(`{"Plugin":{},"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/doubao-work"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var registration struct {
		Routes    []struct{ Method, Path, Menu string }      `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	if err := json.Unmarshal(encoded, &registration); err != nil {
		t.Fatal(err)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/console" || registration.Resources[0].Menu == "" {
		t.Fatal("page resource not declared", string(encoded))
	}
	if len(registration.Routes) != len(managementRoutes) {
		t.Fatal("routes missing", string(encoded))
	}
	for _, route := range registration.Routes {
		// A Menu on a GET route would turn it into an unauthenticated resource.
		if !strings.HasPrefix(route.Path, "/v0/management/doubao-work/") || route.Menu != "" || strings.ContainsAny(route.Path, ":*") {
			t.Fatal("route outside the authenticated management prefix", route)
		}
	}
	register, _ := json.Marshal(object{"config_yaml": []byte("state_dir: /state\nhelper_path: /helper\n")})
	registered, err := dispatch("plugin.register", register)
	if err != nil {
		t.Fatal(err)
	}
	if field(registered, "capabilities", "management_api") != true {
		t.Fatal("management capability not declared", registered)
	}
}

func TestConsolePageCarriesNoData(t *testing.T) {
	page := callManagement(t, "GET", "/v0/resource/plugins/doubao-work/console", nil, "")
	if page.StatusCode != 200 || !strings.HasPrefix(page.Headers.Get("Content-Type"), "text/html") || !strings.Contains(page.Headers.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatal("page not served with its policy", page.StatusCode, page.Headers)
	}
	html := string(page.Body)
	if !strings.Contains(html, "/v0/management/doubao-work") || strings.Contains(html, "http://") && !strings.Contains(html, "http://www.w3.org") {
		t.Fatal("page must call only its own management routes")
	}
	runtime.Lock()
	open := runtime.service != nil
	runtime.Unlock()
	if open {
		t.Fatal("serving the page opened the service")
	}
}

func TestManagementRoutesWithoutOpenService(t *testing.T) {
	if r := callManagement(t, "GET", managementBase+"/tasks", nil, ""); r.StatusCode != 200 || string(r.Body) != `{"tasks":[]}` {
		t.Fatal("tasks without service", r.StatusCode, string(r.Body))
	}
	if r := callManagement(t, "POST", managementBase+"/tasks/cancel", nil, `{"ID":"nope"}`); r.StatusCode != 404 {
		t.Fatal("cancel of unknown task", r.StatusCode)
	}
	if r := callManagement(t, "POST", managementBase+"/tasks/cancel", nil, `not json`); r.StatusCode != 400 {
		t.Fatal("malformed cancel", r.StatusCode)
	}
	if r := callManagement(t, "POST", managementBase+"/test", nil, `{"Model":"","Prompt":""}`); r.StatusCode != 400 {
		t.Fatal("empty test accepted", r.StatusCode)
	}
	if r := callManagement(t, "GET", managementBase+"/nope", nil, ""); r.StatusCode != 404 {
		t.Fatal("unknown route", r.StatusCode)
	}
	if r := callManagement(t, "GET", managementBase+"/login/status", url.Values{"state": {"unknown"}}, ""); r.StatusCode != 400 {
		t.Fatal("unknown login flow", r.StatusCode, string(r.Body))
	}
}

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static cliproxy_host_api stored_host_value;
static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	if (host != NULL) { stored_host_value = *host; stored_host = &stored_host_value; }
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeABI(response, failure("plugin_internal", "plugin internal error", 500))
			status = 1
		}
	}()
	if method == nil || requestLen > 64<<20 {
		writeABI(response, failure("invalid_request", "invalid plugin RPC", 400))
		return 1
	}
	var data []byte
	if request != nil && requestLen > 0 {
		data = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, err := dispatch(C.GoString(method), data)
	if err != nil {
		writeABI(response, failureFrom(err))
		return 1
	}
	raw, err := json.Marshal(map[string]any{"ok": true, "result": result})
	if err != nil {
		writeABI(response, failure("invalid_response", "cannot encode plugin response", 500))
		return 1
	}
	writeABI(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { closePlugin() }

func writeABI(response *C.cliproxy_buffer, data []byte) {
	response.ptr = C.CBytes(data)
	response.len = C.size_t(len(data))
}

func hostCall(method string, request any, result any) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	cm := C.CString(method)
	defer C.free(unsafe.Pointer(cm))
	cp := C.CBytes(data)
	defer C.free(cp)
	var response C.cliproxy_buffer
	status := C.call_host_api(cm, (*C.uint8_t)(cp), C.size_t(len(data)), &response)
	if response.ptr == nil {
		return fmt.Errorf("host callback %s returned no response", method)
	}
	defer C.free_host_buffer(response.ptr, response.len)
	raw := C.GoBytes(response.ptr, C.int(response.len))
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return fmt.Errorf("invalid host callback envelope")
	}
	if status != 0 || !envelope.OK {
		return fmt.Errorf("host callback %s failed", method)
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

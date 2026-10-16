package doubao

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Each call has its own journal and worker. The SSE reader never waits for a
// function result, so observations cannot be serialized by our old dispatcher.
type concurrentTools struct {
	c                                  *Client
	dir, sandbox, workspace, connector string
	forward                            func(context.Context, object, object) (object, error)
	mu                                 sync.Mutex
	seen                               map[string]string
	closing                            bool
	workers                            []*toolProbe
	wg                                 sync.WaitGroup
	errors                             chan error
	completed                          func(object, error)
}

func (d *concurrentTools) handle(ctx context.Context, frame object) error {
	kind := number(frame["event_type"])
	if kind != 1 && kind != 2 {
		return nil
	}
	sandbox := textValue(frame["sandbox_id"])
	if sandbox == "" {
		sandbox = textValue(frame["sandBoxID"])
	}
	if sandbox == "" {
		sandbox = textValue(frame["instance_id"])
	}
	if sandbox != d.sandbox {
		return nil
	}
	key := "tool_call"
	if kind == 2 {
		key = "command"
	}
	call, ok := frame[key].(map[string]any)
	if !ok || textValue(call["id"]) == "" {
		return errors.New("event missing call identity")
	}
	id := fmt.Sprint(kind) + ":" + textValue(call["id"])
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(jsonText(call))))
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil
	}
	if prior, exists := d.seen[id]; exists {
		d.mu.Unlock()
		if prior != fingerprint {
			return errors.New("native call ID collision")
		}
		return nil
	}
	if len(d.seen) >= 24 {
		d.mu.Unlock()
		return errors.New("probe call limit reached")
	}
	d.seen[id] = fingerprint
	journalID := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:20]
	p := &toolProbe{c: d.c, sandbox: d.sandbox, workspace: d.workspace, connector: d.connector,
		journal: filepath.Join(d.dir, "parallel-call-"+journalID+".json"),
		events:  map[string]int{}, records: map[string]object{}, forward: d.forward}
	d.workers = append(d.workers, p)
	if kind == 1 {
		d.wg.Add(1)
	}
	d.mu.Unlock()
	if kind == 2 {
		return p.handle(ctx, frame)
	}
	go func() {
		defer d.wg.Done()
		err := p.handle(ctx, frame)
		if d.completed != nil {
			d.completed(call, err)
		}
		if err != nil {
			select {
			case d.errors <- err:
			default:
			}
		}
	}()
	return nil
}

type burstCall struct {
	Name                         string
	Received, Released, Uploaded time.Time
	Value                        string
}

type burstGroup struct {
	mu                            sync.Mutex
	gate                          chan struct{}
	once                          sync.Once
	timerOnce                     sync.Once
	challenge, connector, receipt string
	calls                         []*burstCall
	byID                          map[string]*burstCall
	hold                          time.Duration
}

func (b *burstGroup) release() { b.once.Do(func() { close(b.gate) }) }

func (b *burstGroup) forward(ctx context.Context, call, input object) (object, error) {
	name := textValue(input["remote_tool_name"])
	args, ok := parsed(input["raw_arguments"]).(map[string]any)
	delay := map[string]time.Duration{"lookup_alpha": 1500 * time.Millisecond,
		"lookup_beta": 200 * time.Millisecond, "lookup_gamma": 700 * time.Millisecond}
	_, known := delay[name]
	if call["name"] != "connector.call" || input["connector_id"] != b.connector || number(input["protocol_version"]) != 1 || !known || !ok || len(args) != 1 || args["challenge"] != b.challenge {
		return nativeToolError(call, "Only the three declared lookup tools with the exact challenge are available."), nil
	}
	if err := awaitAccepted(ctx, b.receipt); err != nil {
		return nil, err
	}
	row := &burstCall{Name: name, Received: time.Now(), Value: strings.TrimPrefix(name, "lookup_") + "_" + token()}
	b.mu.Lock()
	for _, prior := range b.calls {
		if prior.Name == name {
			b.mu.Unlock()
			return nil, errors.New("model repeated an already issued lookup")
		}
	}
	b.calls = append(b.calls, row)
	b.byID[textValue(call["id"])] = row
	if len(b.calls) == 3 {
		b.release()
	}
	b.mu.Unlock()
	b.timerOnce.Do(func() {
		go func() {
			timer := time.NewTimer(b.hold)
			defer timer.Stop()
			select {
			case <-timer.C:
				b.release()
			case <-b.gate:
			case <-ctx.Done():
				b.release()
			}
		}()
	})
	select {
	case <-b.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	timer := time.NewTimer(delay[name])
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	b.mu.Lock()
	row.Released = time.Now()
	b.mu.Unlock()
	return object{"id": call["id"], "name": call["name"], "tool_input": call["input"],
		"raw_result": jsonText(object{"content": []any{object{"type": "text", "text": jsonText(object{"value": row.Value})}}, "isError": false})}, nil
}

func (b *burstGroup) completed(call object, err error) {
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if row := b.byID[textValue(call["id"])]; row != nil {
		row.Uploaded = time.Now()
	}
}

func (b *burstGroup) evidence(reply string) object {
	b.mu.Lock()
	defer b.mu.Unlock()
	want := object{}
	var firstRelease time.Time
	for _, row := range b.calls {
		want[strings.TrimPrefix(row.Name, "lookup_")] = row.Value
		if !row.Released.IsZero() && (firstRelease.IsZero() || row.Released.Before(firstRelease)) {
			firstRelease = row.Released
		}
	}
	before, uploaded := 0, 0
	rows := []any{}
	order := append([]*burstCall{}, b.calls...)
	sort.Slice(order, func(i, j int) bool { return order[i].Uploaded.Before(order[j].Uploaded) })
	uploadOrder := []string{}
	for _, row := range order {
		if !row.Uploaded.IsZero() {
			uploadOrder = append(uploadOrder, row.Name)
		}
	}
	for _, row := range b.calls {
		if !firstRelease.IsZero() && row.Received.Before(firstRelease) {
			before++
		}
		if !row.Uploaded.IsZero() {
			uploaded++
		}
		line := object{"tool": row.Name}
		if len(b.calls) > 0 {
			line["received_ms"] = row.Received.Sub(b.calls[0].Received).Milliseconds()
			if !row.Released.IsZero() {
				line["result_released_ms"] = row.Released.Sub(b.calls[0].Received).Milliseconds()
			}
			if !row.Uploaded.IsZero() {
				line["upload_confirmed_ms"] = row.Uploaded.Sub(b.calls[0].Received).Milliseconds()
			}
		}
		rows = append(rows, line)
	}
	got := parsed(strings.TrimSpace(reply))
	return object{"unique_tools": len(b.calls), "calls_received_before_any_result_released": before,
		"parallel_issuance_observed": before >= 2, "all_three_issued_before_results": before == 3,
		"confirmed_result_uploads": uploaded, "final_exact_match": len(b.calls) == 3 && jsonText(got) == jsonText(want),
		"timeline": rows, "result_upload_confirmation_order": uploadOrder,
		"controlled_first_call_hold_ms": b.hold.Milliseconds(), "random_values_created_after_native_calls": true}
}

func (c *Client) probeParallel(ctx context.Context, model Model, private string, evidence object) (resultErr error) {
	runID := token()
	receiptPath := filepath.Join(private, "parallel-receipt-"+runID+".json")
	workspace := filepath.Join(private, "parallel-workspace-"+runID)
	if err := os.Mkdir(workspace, 0700); err != nil {
		return err
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	t := &gatewayTurn{id: runID, ctx: ctx, receiptPath: receiptPath, statePath: filepath.Join(private, "parallel-state-"+runID+".json")}
	g := &gateway{c: c, dir: private}
	tools := []any{}
	for _, name := range []string{"lookup_alpha", "lookup_beta", "lookup_gamma"} {
		tools = append(tools, object{"name": name, "description": "Independent read-only lookup. Its result does not depend on any other tool; issue all three lookups concurrently before waiting for results.", "inputSchema": jsonText(object{"type": "object", "properties": object{"challenge": object{"type": "string"}}, "required": []string{"challenge"}, "additionalProperties": false})})
	}
	connector, err := g.newConnector(t, tools)
	if connector != "" {
		defer func() {
			cleanupErr := g.removeConnector(t)
			evidence["connector_removed"] = t.cleanupConfirmed
			if cleanupErr != nil {
				evidence["cleanup_error"] = cleanupErr.Error()
				if resultErr == nil {
					resultErr = cleanupErr
				}
			}
		}()
	}
	if err != nil {
		return err
	}
	challenge := "parallel_" + token()[:16]
	prompt := "并行工具调用能力验证。三个函数 lookup_alpha、lookup_beta、lookup_gamma 互相独立。请在同一轮同时发起这三个函数的调用，三个 challenge 参数都必须是 " + challenge + "。必须先发出所有三个调用再等待结果，不要串行等待。所有工具都在 MCP 连接器 " + connector + " 中。不要使用本地 Bash/Read/Write/Edit 或其他工具，也不要模拟工具结果。得到三份真实结果后，最终只输出一行 JSON，格式为 {\"alpha\":lookup_alpha返回的value,\"beta\":lookup_beta返回的value,\"gamma\":lookup_gamma返回的value}，不要解释或代码块。"
	body := localToolBody(prompt, model, workspace, "parallel-"+runID, c.session.DeviceID, connector, tools)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	group := &burstGroup{gate: make(chan struct{}), challenge: challenge, connector: connector, receipt: receiptPath, byID: map[string]*burstCall{}, hold: 12 * time.Second}
	d := &concurrentTools{c: c, dir: private, sandbox: "parallel-" + runID, workspace: workspace, connector: connector,
		forward: group.forward, seen: map[string]string{}, errors: make(chan error, 1), completed: group.completed}
	wrote := make(chan struct{})
	var once sync.Once
	channelCtx := httptrace.WithClientTrace(runCtx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			once.Do(func() { close(wrote) })
		}
	}})
	done := make(chan error, 1)
	go func() {
		response, e := c.request(channelCtx, "POST", "/alice/office/tool_local/chunk_stream", url.Values{"sand_box_id": []string{d.sandbox}, "client_version": []string{"unspecified"}}, object{"device_id": c.session.DeviceID, "client_attrs": object{"sandbox_id": d.sandbox}})
		if e == nil {
			if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				e = errors.New("tool channel is not SSE")
			} else {
				e = consumeSSE(response.Body, func(_ string, frame object, _ string) (bool, error) { return false, d.handle(runCtx, frame) })
			}
			response.Body.Close()
		}
		if runCtx.Err() == nil {
			cancel()
		}
		done <- e
	}()
	select {
	case <-wrote:
	case e := <-done:
		return e
	case <-runCtx.Done():
		return runCtx.Err()
	}
	go func() {
		select {
		case <-d.errors:
			cancel()
		case <-runCtx.Done():
		}
	}()
	evidence["generation_posts"] = 1
	receipt, runErr := c.run(runCtx, body, receiptPath)
	if runErr != nil {
		cancel()
	}
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
	d.wg.Wait()
	cancel()
	<-done
	if receipt != nil {
		evidence["accepted"], evidence["server_completed"] = receipt.Accepted, receipt.Completed
		evidence["parallel"] = group.evidence(receipt.Reply)
	}
	if runErr != nil {
		if receipt != nil && receipt.Accepted && !receipt.Completed {
			stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			stopErr := c.stopProbe(stopCtx, receiptPath, object{"action": "tool-stop"})
			stop()
			evidence["cancellation_confirmed"] = stopErr == nil
		}
		return runErr
	}
	if receipt == nil || !receipt.Completed || field(evidence, "parallel", "final_exact_match") != true {
		return errors.New("parallel probe did not verify all three actual tool results")
	}
	evidence["success"] = true
	fmt.Println("PARALLEL_PROBE_VERIFIED", jsonText(evidence))
	return nil
}

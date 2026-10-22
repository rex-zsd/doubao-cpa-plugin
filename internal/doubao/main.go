package doubao

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func RunCLI() (resultErr error) {
	action := flag.String("action", "verify", "bootstrap, login, models, verify, readback, tool-probe, parallel-probe, media-probe, tool-stop, tool-status, serve, gateway-audit, mcp-nonce, mcp-describe")
	dir := flag.String("dir", "private", "private session and receipt directory")
	out := flag.String("out", "evidence", "sanitized evidence directory")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout")
	requestedModel := flag.String("model", "", "exact live model name; empty chooses an available model in the selected mode")
	receiptFile := flag.String("receipt", "", "existing private receipt for readback; never submits a new message")
	mode := flag.String("mode", "auto", "auto, chat or work")
	listen := flag.String("listen", "127.0.0.1:18321", "loopback HTTP gateway address")
	taskTimeout := flag.Duration("task-timeout", 5*time.Minute, "gateway task lifetime, including client tool execution")
	taskID := flag.String("task-id", "", "saved native task identifier")
	filePath := flag.String("file", "", "local file to register")
	fileID := flag.String("file-id", "", "account-local file identifier")
	toolSchema := flag.String("tool-schema", "", "private MCP descriptor schema file")
	nativeTools := flag.Bool("native-tools", false, "forward compatible native text Read/Edit/Write and foreground Bash to the HTTP caller; other tools retain MCP")
	legacyNativeProbe := flag.Bool("native-tools-probe", false, "alias for -native-tools (legacy probe scripts)")
	flag.Parse()
	if *action == "mcp-nonce" {
		return serveNonce(os.Stdin, os.Stdout)
	}
	if *action == "mcp-describe" {
		return serveToolDescriptor(os.Stdin, os.Stdout, *toolSchema)
	}
	if *mode != "auto" && *mode != "chat" && *mode != "work" {
		return errors.New("invalid mode")
	}
	private, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	public, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	c, err := newClient(private)
	if err != nil {
		return err
	}
	switch *action {
	case "task-list":
		paths, err := filepath.Glob(filepath.Join(private, "gateway-state-*.json"))
		if err != nil {
			return err
		}
		tasks := []any{}
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var state object
			if err = decode(strings.NewReader(string(b)), &state); err != nil {
				return err
			}
			tasks = append(tasks, object{"task_id": state["id"], "done": state["done"], "generation_posts": state["generation_posts"], "forwarded_tools": state["forwarded_tools"], "cleanup_confirmed": state["cleanup_confirmed"], "error": state["error"]})
		}
		fmt.Println(jsonText(object{"data": tasks}))
		return nil
	case "task-cancel":
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		if len(*taskID) != 32 || strings.Trim(*taskID, "0123456789abcdef") != "" {
			return errors.New("invalid task-id")
		}
		evidence := object{"action": "tool-stop", "success": false}
		err := c.stopProbe(ctx, filepath.Join(private, "gateway-receipt-"+*taskID+".json"), evidence)
		if err == nil {
			fmt.Println(jsonText(evidence))
		}
		return err
	case "file-put":
		f, err := os.Open(*filePath)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, mediaMaxBytes+1))
		if err != nil {
			return err
		}
		id, err := c.storeFile(filepath.Base(*filePath), data)
		if err == nil {
			fmt.Println(jsonText(object{"id": id, "filename": filepath.Base(*filePath), "bytes": len(data)}))
		}
		return err
	case "file-list":
		files, err := c.listFiles()
		if err == nil {
			fmt.Println(jsonText(object{"data": files}))
		}
		return err
	case "file-delete":
		err := c.deleteFile(*fileID)
		if err == nil {
			fmt.Println(jsonText(object{"id": *fileID, "deleted": true}))
		}
		return err
	}
	if *action == "serve" {
		return serveGateway(c, private, *listen, *taskTimeout, *nativeTools || *legacyNativeProbe)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	evidence := object{"implementation": "independent Go standard library", "reference_library_used": false, "started_at": time.Now().Format(time.RFC3339), "action": *action, "success": false}
	defer func() {
		if resultErr != nil {
			evidence["error"] = resultErr.Error()
		}
		evidence["finished_at"] = time.Now().Format(time.RFC3339)
		if err := writeEvidence(filepath.Join(public, "latest.json"), evidence); err != nil {
			fmt.Fprintln(os.Stderr, "evidence persistence failed")
		}
	}()
	if *action == "bootstrap" {
		r, err := c.request(ctx, "GET", "/", nil, nil)
		if err != nil {
			evidence["error"] = err.Error()
			return err
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
		r.Body.Close()
		if readErr != nil {
			return errors.New("bootstrap body read failed")
		}
		evidence["success"] = true
		evidence["http_status"] = r.StatusCode
		evidence["cookie_count"] = len(c.jar.snapshot())
		fmt.Println(jsonText(evidence))
		return nil
	}
	if *action == "login" {
		err = c.login(ctx)
		if err != nil {
			evidence["error"] = err.Error()
			return err
		}
		evidence["success"] = true
		evidence["authenticated"] = true
		return nil
	}
	if *action == "tool-stop" || *action == "tool-status" {
		if *receiptFile == "" {
			return errors.New("tool-stop/tool-status requires -receipt")
		}
		return c.stopProbe(ctx, *receiptFile, evidence)
	}
	if *action == "gateway-audit" {
		return c.auditGateway(ctx, private, evidence)
	}
	if *action == "readback" {
		if *receiptFile == "" {
			return errors.New("readback requires -receipt")
		}
		file, err := os.Open(*receiptFile)
		if err != nil {
			return err
		}
		defer file.Close()
		var r Receipt
		if err := decode(file, &r); err != nil {
			return err
		}
		if !r.Accepted || r.ConversationID == "" || r.RunID == "" {
			return errors.New("readback requires an accepted receipt")
		}
		if err := c.account(ctx); err != nil {
			return err
		}
		for {
			answer, done, err := c.readReply(ctx, &r)
			if err != nil {
				return err
			}
			if done {
				r.Reply, r.Completed = answer, true
				if err := saveJSON(*receiptFile, &r); err != nil {
					return err
				}
				messages := array(r.Request["messages"])
				prompt := ""
				if len(messages) > 0 {
					blocks := array(field(messages[0], "content_block"))
					if len(blocks) > 0 {
						prompt = textValue(field(blocks[0], "content", "text_block", "text"))
					}
				}
				marker := ""
				if n := strings.Index(prompt, "DOUBAO_POC_"); n >= 0 {
					marker = prompt[n:]
				}
				evidence["accepted"], evidence["server_completed"], evidence["reply"], evidence["expected_marker"], evidence["marker_exact_match"], evidence["event_counts"] = true, true, answer, marker, marker != "" && answer == marker, r.Events
				if marker == "" || answer != marker {
					return errors.New("readback marker mismatch")
				}
				evidence["success"] = true
				fmt.Println("READBACK_VERIFIED", jsonText(evidence))
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if *action != "models" && *action != "verify" && *action != "tool-probe" && *action != "parallel-probe" && *action != "media-probe" {
		return errors.New("unsupported action")
	}
	if c.session.AccountID == "" {
		err = c.login(ctx)
	} else {
		err = c.account(ctx)
	}
	if err != nil {
		evidence["error"] = err.Error()
		return err
	}
	evidence["authenticated"] = true
	models, err := c.models(ctx)
	if err != nil {
		evidence["error"] = err.Error()
		return err
	}
	evidence["model_count"] = len(models)
	if err := writeEvidence(filepath.Join(public, "models.json"), models); err != nil {
		return err
	}
	fmt.Printf("MODEL_CATALOG_OK count=%d\n", len(models))
	if *action == "models" {
		evidence["success"] = true
		return nil
	}
	selectedMode := *mode
	if *action == "tool-probe" || *action == "parallel-probe" || *action == "media-probe" {
		if *mode == "chat" {
			return errors.New("tool probe requires Work mode")
		}
		selectedMode = "work"
	}
	selected, selectedMode, err := selectModel(models, *requestedModel, selectedMode)
	if err != nil {
		return err
	}
	evidence["model"] = selected.Name
	evidence["mode"] = selectedMode
	if *action == "parallel-probe" {
		return c.probeParallel(ctx, selected, private, evidence)
	}
	if *action == "media-probe" {
		return c.probeMedia(ctx, selected, private, public, evidence)
	}
	if *action == "tool-probe" {
		if err := c.probeTools(ctx, selected, private, public, evidence); err != nil {
			return err
		}
		evidence["success"] = true
		fmt.Println("TOOL_LOOP_VERIFIED", jsonText(evidence))
		return nil
	}
	marker := "DOUBAO_POC_" + token()[:12]
	body := buildBody("这是接口连通性验证。请只原样回复以下字符串，不要解释，不要调用工具："+marker, "", selectedMode, selected)
	receipt, err := c.run(ctx, body, filepath.Join(private, "receipt-"+token()+".json"))
	if receipt != nil {
		evidence["accepted"] = receipt.Accepted
		evidence["server_completed"] = receipt.Completed
		evidence["event_counts"] = receipt.Events
		evidence["task_count"] = len(receipt.Tasks)
		evidence["reply"] = receipt.Reply
		evidence["expected_marker"] = marker
		evidence["marker_exact_match"] = receipt.Reply == marker
	}
	if err != nil {
		evidence["error"] = err.Error()
		return err
	}
	if !receipt.Completed || receipt.Reply != marker {
		return errors.New("server reply did not exactly match the generated marker")
	}
	evidence["success"] = true
	fmt.Println("LIVE_VERIFICATION_OK", jsonText(evidence))
	return nil
}

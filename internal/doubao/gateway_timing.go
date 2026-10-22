package doubao

import (
	"fmt"
	"os"
	"time"
)

// Timing contains only an opaque task id and durations, never prompts, tool
// arguments or credentials. Zero timestamps (e.g. a restored pending tool)
// mean the duration is unknown, rather than including time before recovery.
// It reads immutable fields and is safe to call with or without g.mu held.
func timing(t *gatewayTurn, stage string, since time.Time) {
	elapsed := func(start time.Time) int64 {
		if start.IsZero() {
			return -1
		}
		return time.Since(start).Milliseconds()
	}
	fmt.Fprintf(os.Stderr, "GATEWAY_TIMING task=%s stage=%s elapsed_ms=%d duration_ms=%d\n", t.id, stage, elapsed(t.started), elapsed(since))
}

// Observing the ACK does not change the upstream request or expose any of
// its contents. Acceptance is still verified and persisted by runObserved.
func timingObserver(t *gatewayTurn, observe func(string, object) error) func(string, object) error {
	started, ack, chunk := time.Now(), false, false
	var readback time.Time
	return func(name string, data object) error {
		switch name {
		case "STREAM_CHUNK":
			if !chunk {
				chunk = true
				timing(t, "upstream_first_chunk", started)
			}
		case "STREAM_TIMEOUT_CONTROL":
			timing(t, "upstream_timeout_control", started)
		case "GATEWAY_STREAM_FINISHED":
			readback = time.Now()
			timing(t, "upstream_stream_finished", started)
		case "GATEWAY_READBACK_FINISHED":
			timing(t, "final_readback_done", readback)
		}
		if name == "SSE_ACK" && !ack {
			ack = true
			timing(t, "upstream_ack_received", started)
		}
		if observe != nil {
			return observe(name, data)
		}
		return nil
	}
}

// Finish publishes a durable result before this cleanup runs. The caller is
// the existing tracked task worker, so Close still joins it. Pending cleanup
// pins the turn/workspace and is recovered after a restart. An unfinished
// suspended task must keep its connector so recovery can resume its tools.
func (g *gateway) cleanupFinished(t *gatewayTurn) {
	g.mu.Lock()
	needed := t.done && (t.connector != "" || t.connectorLease != "") && !t.cleanupConfirmed
	g.mu.Unlock()
	if !needed {
		return
	}
	started := time.Now()
	timing(t, "cleanup_start", started)
	if err := g.removeConnector(t); err != nil {
		timing(t, "cleanup_pending", started)
		fmt.Fprintln(os.Stderr, "GATEWAY_CONNECTOR_CLEANUP_PENDING", err.Error())
		return
	}
	timing(t, "cleanup_done", started)
}

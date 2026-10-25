package doubao

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func consoleService(g *gateway) *Service { return &Service{c: g.c, g: g} }

// The page sees which caller tool a task waits on and how tasks ended, never
// the conversation itself.
func TestConsoleTasksAndStatsReportMetadataOnly(t *testing.T) {
	g, server := testGateway(t)
	var starts atomic.Int32
	g.run = chainRunner(g, &starts, 1, instant)
	s := consoleService(g)
	req := toolRequest()
	req.Messages[0].Content = "SECRET-PROMPT-TEXT"
	code, exposed := postGateway(t, server, req, "")
	if code != 200 {
		t.Fatal(code, exposed)
	}
	tasks := s.Tasks()
	if len(tasks) != 1 || tasks[0].State != "waiting_tool" || tasks[0].Tool != "client_nonce" || tasks[0].Model != "TestModel" || tasks[0].Deadline.IsZero() {
		t.Fatalf("waiting task not described: %+v", tasks)
	}
	if st := s.Stats(); st.Active != 1 || st.Limit != activeTaskLimit || st.LastRequest.IsZero() || st.Finished != 0 {
		t.Fatalf("stats while running: %+v", st)
	}
	code, answer := postGateway(t, server, answerTool(t, req, exposed, "SECRET-TOOL-RESULT"), "")
	if code != 200 {
		t.Fatal(code, answer)
	}
	g.wg.Wait()
	tasks = s.Tasks()
	if len(tasks) != 1 || tasks[0].State != "done" || tasks[0].Outcome != "ok" || tasks[0].Forwarded != 1 {
		t.Fatalf("finished task not described: %+v", tasks)
	}
	st := s.Stats()
	if st.Active != 0 || st.Finished != 1 || st.Outcomes["ok"] != 1 {
		t.Fatalf("stats after finish: %+v", st)
	}
	if text := jsonText(object{"tasks": tasks, "stats": st}); strings.Contains(text, "SECRET") {
		t.Fatal("page data carries conversation content", text)
	}
}

// Cancelling stops the task and its waiting caller; a tool result sent for it
// afterwards starts a new task over the full conversation instead of failing.
func TestConsoleCancelThenLateResultStartsNewTask(t *testing.T) {
	g, server := testGateway(t)
	var starts atomic.Int32
	g.run = chainRunner(g, &starts, 1, instant)
	s := consoleService(g)
	req := toolRequest()
	_, exposed := postGateway(t, server, req, "")
	tasks := s.Tasks()
	if len(tasks) != 1 || !s.CancelTask(tasks[0].ID) {
		t.Fatal("running task could not be cancelled", tasks)
	}
	if s.CancelTask(tasks[0].ID) || s.CancelTask("unknown") {
		t.Fatal("cancel accepted a finished or unknown task")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Stats().Outcomes["task_cancelled"] != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Stats().Outcomes["task_cancelled"] != 1 {
		t.Fatal("cancelled task did not finish as task_cancelled", s.Stats())
	}
	code, next := postGateway(t, server, answerTool(t, req, exposed, "late result"), "")
	if code != 200 || starts.Load() != 2 {
		t.Fatal("late result did not start a new task", code, starts.Load(), next)
	}
}

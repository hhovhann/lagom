package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplayRestoresEventsAndSkipsTornLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		l.Emit(Event{TS: time.Now(), Kind: "task", ID: "id", Task: "classify", Learn: true, Attempts: []Attempt{{Model: "m", Verdict: "pass"}}})
	}
	l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"ts":"2026-01-01T00:00:00Z","kind":"ta`) // torn final write
	f.Close()

	var got []Event
	n, err := Replay(path, func(e Event) { got = append(got, e) })
	if err != nil || n != 3 || len(got) != 3 || got[0].Task != "classify" || !got[0].Learn {
		t.Fatalf("replay = %d events, err %v: %+v", n, err, got)
	}
	if n, err := Replay(filepath.Join(t.TempDir(), "missing.jsonl"), nil); n != 0 || err != nil {
		t.Errorf("a missing log is not an error, got %d, %v", n, err)
	}
}

func TestResetStartsFreshLogAndKeepsTheOldOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, _ := New(path, nil)
	l.Emit(Event{TS: time.Now(), Kind: "task", Task: "a"})
	l.Reset()
	l.Emit(Event{TS: time.Now(), Kind: "task", Task: "b"})
	l.Close()
	var tasks []string
	Replay(path, func(e Event) { tasks = append(tasks, e.Task) })
	if len(tasks) != 1 || tasks[0] != "b" {
		t.Errorf("after reset only new events replay, got %v", tasks)
	}
	if _, err := os.Stat(path + ".reset"); err != nil {
		t.Errorf("the old log must be kept: %v", err)
	}
}

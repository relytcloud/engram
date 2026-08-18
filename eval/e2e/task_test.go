package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTask(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validTask = `{"id":"arch-001","category":"architecture-qa",
"prompt":"Where does ZDB store visimap metadata and how is it GC'd?",
"rubric":{"answer_points":["visimap keys live in FDB","GC via drop flags"],"gotcha_points":[],"max_score":10},
"max_turns":30,"timeout_min":20}`

func TestLoadTasksValid(t *testing.T) {
	dir := t.TempDir()
	writeTask(t, dir, "arch-001.json", validTask)
	tasks, err := LoadTasks(dir)
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "arch-001" || tasks[0].Rubric.MaxScore != 10 {
		t.Errorf("parsed wrong: %+v", tasks)
	}
}

func TestLoadTasksRejectsBad(t *testing.T) {
	bad := []string{
		`{"id":"","category":"c","prompt":"p","rubric":{"answer_points":["a"],"max_score":10},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"","rubric":{"answer_points":["a"],"max_score":10},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"p","rubric":{"answer_points":[],"max_score":10},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"p","rubric":{"answer_points":["a"],"max_score":5},"max_turns":30,"timeout_min":20}`,
		`{"id":"x","category":"c","prompt":"p","rubric":{"answer_points":["a"],"max_score":10},"max_turns":0,"timeout_min":20}`,
	}
	for i, content := range bad {
		dir := t.TempDir()
		writeTask(t, dir, "t.json", content)
		if _, err := LoadTasks(dir); err == nil {
			t.Errorf("bad[%d]: expected error", i)
		}
	}
}

func TestRealTaskSetParses(t *testing.T) {
	tasks, err := LoadTasks("../datasets/phoenix-e2e-v1/tasks")
	if err != nil {
		t.Fatalf("real task set invalid: %v", err)
	}
	if len(tasks) < 12 || len(tasks) > 16 {
		t.Errorf("task count %d outside 12-16", len(tasks))
	}
}

func TestFilterTasks(t *testing.T) {
	tasks := []Task{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got, err := FilterTasks(tasks, "c,a")
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("FilterTasks = %+v, %v", got, err)
	}
	if all, err := FilterTasks(tasks, ""); err != nil || len(all) != 3 {
		t.Fatalf("empty csv should return all, got %d, %v", len(all), err)
	}
	if _, err := FilterTasks(tasks, "a,zzz"); err == nil {
		t.Fatal("unknown id must error")
	}
	// Trailing (and interior) empty entries are tolerated, not reported as an
	// unknown empty id.
	trailing, err := FilterTasks(tasks, "a,")
	if err != nil || len(trailing) != 1 || trailing[0].ID != "a" {
		t.Fatalf("trailing comma: got %+v, %v", trailing, err)
	}
	if got, err := FilterTasks(tasks, "a,,b"); err != nil || len(got) != 2 {
		t.Fatalf("interior empty entry: got %+v, %v", got, err)
	}
	if got, err := FilterTasks(tasks, " , "); err != nil || len(got) != 3 {
		t.Fatalf("all-empty csv should return all, got %+v, %v", got, err)
	}
}

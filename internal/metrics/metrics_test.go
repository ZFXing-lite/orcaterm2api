package metrics

import (
	"path/filepath"
	"testing"
)

func TestSnapshotTotals(t *testing.T) {
	tr := New(true)
	defer tr.Close()
	tr.Record("acct1", "m1", 100, 50)
	tr.Record("acct1", "m2", 10, 5)
	tr.RecordError("acct1")

	snap := tr.Snapshot()
	tt, ok := snap["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals 缺失: %+v", snap)
	}
	if tt["prompt_tokens"] != int64(110) || tt["completion_tokens"] != int64(55) || tt["total_tokens"] != int64(165) {
		t.Errorf("token 汇总错误: %+v", tt)
	}
	if tt["requests"] != int64(2) || tt["errors"] != int64(1) {
		t.Errorf("请求/错误汇总错误: %+v", tt)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	tr := New(true)
	tr.Record("a", "m", 7, 3)
	tr.save(path)

	tr2 := New(true)
	defer tr2.Close()
	if err := tr2.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	tt := tr2.Snapshot()["totals"].(map[string]any)
	if tt["total_tokens"] != int64(10) || tt["prompt_tokens"] != int64(7) || tt["completion_tokens"] != int64(3) {
		t.Errorf("重启回读 totals 错误: %+v", tt)
	}
	// 续账后再记一笔，汇总应叠加而非清零
	tr2.Record("a", "m", 1, 1)
	tt2 := tr2.Snapshot()["totals"].(map[string]any)
	if tt2["total_tokens"] != int64(12) {
		t.Errorf("续账叠加错误(7+3+1+1=12): %+v", tt2)
	}
}

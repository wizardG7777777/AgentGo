package bootstrap

import (
	"fmt"
	"os"
	"testing"

	"agentgo/internal/trace"
)

func TestTraceArchiveRetainsFilesAcrossWriterRebind(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"", 100}, {"1", 120}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("AGENTGO_TRACE_KEEP_ALL", tc.value)
			dir := t.TempDir()
			for generation := 0; generation < 2; generation++ {
				writer, err := trace.NewWriter(dir, traceFileLimit())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { writer.Close() })
				for i := 0; i < 60; i++ {
					id := fmt.Sprintf("%08d", generation*60+i)
					writer.Emit(trace.Event{Kind: trace.KindTaskClaimed, TaskID: id})
					writer.CloseTask(id)
				}
				writer.Close()
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != tc.want {
				t.Fatalf("retained %d files, want %d", len(files), tc.want)
			}
		})
	}
}

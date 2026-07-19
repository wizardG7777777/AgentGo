package trace

import (
	"strings"
	"testing"
)

func TestFormatTaskPublishedIncludesLineage(t *testing.T) {
	details := formatEventDetails(Event{
		Kind: KindTaskPublished, ParentTaskID: "parent-1", BatchID: "batch-1",
	})
	for _, want := range []string{"parent=parent-1", "batch=batch-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("formatEventDetails() = %q, missing %q", details, want)
		}
	}
}

package mailbox

import "testing"

func TestResolveReplyRecipient(t *testing.T) {
	r := NewRegistry(4)
	r.Register("explicit-agent", "")
	r.Register("legacy-agent", "")
	r.Register("parent-task-uuid", "")
	r.RegisterAlias("scheduler", "explicit-agent")

	tests := []struct {
		name      string
		reply     string
		legacy    string
		forbidden []string
		want      string
	}{
		{name: "explicit wins", reply: "explicit-agent", legacy: "legacy-agent", want: "explicit-agent"},
		{name: "explicit alias", reply: "scheduler", legacy: "legacy-agent", want: "scheduler"},
		{name: "legacy registered fallback", reply: "missing", legacy: "legacy-agent", want: "legacy-agent"},
		{name: "unknown legacy rejected", legacy: "task-that-is-not-a-mailbox"},
		{name: "user rejected", legacy: "user"},
		{name: "parent task forbidden even if mailbox exists", legacy: "parent-task-uuid", forbidden: []string{"current-task", "parent-task-uuid"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.ResolveReplyRecipient(tc.reply, tc.legacy, tc.forbidden...); got != tc.want {
				t.Fatalf("ResolveReplyRecipient() = %q, want %q", got, tc.want)
			}
		})
	}
}

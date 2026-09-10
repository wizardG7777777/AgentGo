package tools

// fakeFinalizationNotifier 记录统一结果提交是否进入收尾。
type fakeFinalizationNotifier struct{ marked bool }

func (f *fakeFinalizationNotifier) MarkTaskFinalized() { f.marked = true }

func (f *fakeFinalizationNotifier) IsFinalized() bool { return f.marked }

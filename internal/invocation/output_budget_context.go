package invocation

import (
	"context"
	"fmt"
)

type outputBudgetContextKey struct{}

func WithOutputBudget(ctx context.Context, budget OutputBudget) (context.Context, error) {
	if err := budget.Validate(); err != nil {
		return nil, fmt.Errorf("冻结 action OutputBudget: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, outputBudgetContextKey{}, budget.Clone()), nil
}

func OutputBudgetFrom(ctx context.Context) (OutputBudget, bool) {
	if ctx == nil {
		return OutputBudget{}, false
	}
	budget, ok := ctx.Value(outputBudgetContextKey{}).(OutputBudget)
	return budget.Clone(), ok
}

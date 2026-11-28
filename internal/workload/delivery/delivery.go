package delivery

import "context"

type resumedKey struct{}

func WithResumed(ctx context.Context) context.Context {
	return context.WithValue(ctx, resumedKey{}, true)
}

func Resumed(ctx context.Context) bool {
	resumed, _ := ctx.Value(resumedKey{}).(bool)
	return resumed
}

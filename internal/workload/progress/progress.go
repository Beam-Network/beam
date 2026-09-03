package progress

import "context"

type Reporter func(map[string]string)

type reporterKey struct{}

func WithReporter(ctx context.Context, reporter Reporter) context.Context {
	return context.WithValue(ctx, reporterKey{}, reporter)
}

func Report(ctx context.Context, outputs map[string]string) {
	reporter, _ := ctx.Value(reporterKey{}).(Reporter)
	if reporter != nil {
		reporter(outputs)
	}
}

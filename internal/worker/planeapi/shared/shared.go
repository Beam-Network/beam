package shared

import (
	"context"
	"net"
	"runtime"
	"runtime/pprof"
)

func Do(f func()) {
	done := make(chan struct{})
	var (
		recovered any
		panicked  bool
	)
	go func() {
		defer close(done)
		defer func() {
			if value := recover(); panicked {
				recovered = value
			}
		}()
		pprof.SetGoroutineLabels(context.Background())
		panicked = true
		f()
		panicked = false
	}()
	<-done
	if panicked {
		if recovered == nil {
			runtime.Goexit()
		}
		panic(recovered)
	}
}

func Go(f func()) {
	Do(func() { go f() })
}

func TransportDial(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		pprof.SetGoroutineLabels(context.Background())
		return dial(ctx, network, address)
	}
}

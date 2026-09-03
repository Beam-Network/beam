// Package fanout provides a small, reusable source-to-branches execution
// kernel. Drivers own transport, data validation, and proof formats.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Driver[S, B, R any] interface {
	BranchID(B) string
	GroupKey(B) string
	Read(context.Context, S, B) ([]byte, error)
	Deliver(context.Context, B, []byte) (R, error)
}

type Lifecycle[B, R any] struct {
	GroupStarted   func(index, total int, exemplar B)
	BranchComplete func(branch B, result R, completed int) error
	BranchFailed   func(branch B, err error)
}

type Config struct {
	MaxConcurrentBranches int
	QueueDepth            int
	ContinueOnBranchError bool
}

type FanoutEngine[S, B, R any] struct {
	config Config
	driver Driver[S, B, R]
}

type Result[R any] struct {
	Branches  map[string]R
	Failures  map[string]error
	BytesRead int64
}

func NewFanoutEngine[S, B, R any](config Config, driver Driver[S, B, R]) (*FanoutEngine[S, B, R], error) {
	if driver == nil {
		return nil, errors.New("fanout driver is required")
	}
	if config.MaxConcurrentBranches <= 0 {
		config.MaxConcurrentBranches = 16
	}
	if config.QueueDepth <= 0 {
		config.QueueDepth = config.MaxConcurrentBranches
	}
	return &FanoutEngine[S, B, R]{config: config, driver: driver}, nil
}

// Run reads each distinct source group once and delivers it through a bounded
// branch queue. completed is copied, allowing durable callers to resume without
// repeating acknowledged branch I/O.
func (e *FanoutEngine[S, B, R]) Run(ctx context.Context, source S, branches []B,
	completed map[string]R, lifecycle Lifecycle[B, R]) (Result[R], error) {
	result := Result[R]{Branches: make(map[string]R, len(completed)), Failures: make(map[string]error)}
	for id, value := range completed {
		result.Branches[id] = value
	}
	groups := e.pendingGroups(branches, result.Branches)
	for index, group := range groups {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if lifecycle.GroupStarted != nil {
			lifecycle.GroupStarted(index, len(groups), group[0])
		}
		payload, err := e.driver.Read(ctx, source, group[0])
		if err != nil {
			return result, err
		}
		result.BytesRead += int64(len(payload))
		groupContext, cancelGroup := context.WithCancel(ctx)
		outcomes := e.deliverGroup(groupContext, group, payload)
		var failures []error
		for outcome := range outcomes {
			if outcome.err != nil {
				failures = append(failures, fmt.Errorf("branch %s: %w", outcome.id, outcome.err))
				result.Failures[outcome.id] = outcome.err
				if lifecycle.BranchFailed != nil {
					lifecycle.BranchFailed(outcome.branch, outcome.err)
				}
				continue
			}
			result.Branches[outcome.id] = outcome.result
			if lifecycle.BranchComplete != nil {
				if err := lifecycle.BranchComplete(outcome.branch, outcome.result, len(result.Branches)); err != nil {
					cancelGroup()
					for range outcomes {
					}
					return result, err
				}
			}
		}
		cancelGroup()
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := errors.Join(failures...); err != nil && !e.config.ContinueOnBranchError {
			return result, err
		}
	}
	return result, nil
}

func (e *FanoutEngine[S, B, R]) pendingGroups(branches []B, completed map[string]R) [][]B {
	order := make([]string, 0)
	grouped := make(map[string][]B)
	for _, branch := range branches {
		if _, ok := completed[e.driver.BranchID(branch)]; ok {
			continue
		}
		key := e.driver.GroupKey(branch)
		if _, ok := grouped[key]; !ok {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], branch)
	}
	result := make([][]B, 0, len(order))
	for _, key := range order {
		result = append(result, grouped[key])
	}
	return result
}

type branchOutcome[B, R any] struct {
	id     string
	branch B
	result R
	err    error
}

func (e *FanoutEngine[S, B, R]) deliverGroup(ctx context.Context, branches []B,
	payload []byte) <-chan branchOutcome[B, R] {
	jobs := make(chan B, e.config.QueueDepth)
	outcomes := make(chan branchOutcome[B, R], min(len(branches), e.config.QueueDepth))
	workers := min(len(branches), e.config.MaxConcurrentBranches)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for branch := range jobs {
				value, err := e.driver.Deliver(ctx, branch, payload)
				outcome := branchOutcome[B, R]{id: e.driver.BranchID(branch), branch: branch, result: value, err: err}
				select {
				case outcomes <- outcome:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, branch := range branches {
			select {
			case jobs <- branch:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		group.Wait()
		close(outcomes)
	}()
	return outcomes
}

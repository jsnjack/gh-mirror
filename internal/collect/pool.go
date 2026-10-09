package collect

import (
	"context"
	"fmt"
	"sync"
)

func parallelFetch[T any](ctx context.Context, limit, count int, fetch func(context.Context, int) (T, error), apply func(T) error) error {
	if count == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	jobs := make(chan int)
	results := make(chan result, limit)
	dispatched := make(chan struct{})
	var workers sync.WaitGroup
	for range min(limit, count) {
		workers.Go(func() {
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				value, err := fetch(ctx, index)
				select {
				case results <- result{value, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		})
	}
	go func() {
		defer close(dispatched)
		defer close(jobs)
		for index := range count {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		<-dispatched
		close(results)
	}()
	var first error
	for result := range results {
		if first != nil {
			continue
		}
		if result.err != nil {
			first = result.err
		} else if ctx.Err() != nil {
			first = fmt.Errorf("cancel parallel fetch: %w", ctx.Err())
		} else {
			first = apply(result.value)
		}
		if first != nil {
			cancel()
		}
	}
	if first != nil {
		return first
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cancel parallel fetch: %w", err)
	}
	return nil
}

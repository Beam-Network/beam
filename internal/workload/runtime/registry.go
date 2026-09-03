package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Beam-Network/beam/internal/workload/domain"
)

var ErrHandlerNotFound = errors.New("workload handler not found")

type Handler interface {
	Kind() domain.Kind
	Validate(domain.Spec) error
	Execute(context.Context, domain.Spec) (domain.Result, error)
}

type Registry struct {
	mu       sync.RWMutex
	handlers map[domain.Kind]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: make(map[domain.Kind]Handler)}
}

func (r *Registry) Register(handler Handler) error {
	if handler == nil {
		return errors.New("handler is required")
	}
	kind := handler.Kind()
	if kind == "" {
		return errors.New("handler kind is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[kind]; exists {
		return fmt.Errorf("handler for %q is already registered", kind)
	}
	r.handlers[kind] = handler
	return nil
}

func (r *Registry) Handler(kind domain.Kind) (Handler, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrHandlerNotFound, kind)
	}
	return handler, nil
}

func (r *Registry) Kinds() []domain.Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]domain.Kind, 0, len(r.handlers))
	for kind := range r.handlers {
		kinds = append(kinds, kind)
	}
	return kinds
}

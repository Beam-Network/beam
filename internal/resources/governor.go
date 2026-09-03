package resources

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	workload "github.com/Beam-Network/beam/internal/workload/domain"
)

var (
	ErrInsufficientCapacity = errors.New("insufficient resource capacity")
	ErrReservationConflict  = errors.New("resource reservation conflict")
	ErrReservationNotFound  = errors.New("resource reservation not found")
)

type ReservationState string

const (
	ReservationPending   ReservationState = "pending"
	ReservationCommitted ReservationState = "committed"
)

type Reservation struct {
	Key       string
	Resources workload.Resources
	State     ReservationState
}

type Snapshot struct {
	Capacity  workload.Resources
	Reserved  workload.Resources
	Available workload.Resources
	Pending   int
	Committed int
}

type Governor struct {
	mu           sync.Mutex
	capacity     workload.Resources
	reservations map[string]Reservation
}

func NewGovernor(capacity workload.Resources) (*Governor, error) {
	if err := capacity.Validate(); err != nil {
		return nil, fmt.Errorf("invalid capacity: %w", err)
	}
	return &Governor{capacity: capacity, reservations: make(map[string]Reservation)}, nil
}

func (g *Governor) Reserve(key string, required workload.Resources) error {
	if key == "" {
		return errors.New("reservation key is required")
	}
	if err := required.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if current, ok := g.reservations[key]; ok {
		if reflect.DeepEqual(current.Resources, required) {
			return nil
		}
		return ErrReservationConflict
	}
	available := g.capacity.SubFloor(g.reservedLocked())
	if !available.Fits(required) {
		return fmt.Errorf("%w: available=%+v required=%+v", ErrInsufficientCapacity, available, required)
	}
	g.reservations[key] = Reservation{Key: key, Resources: required, State: ReservationPending}
	return nil
}

func (g *Governor) Commit(key string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	reservation, ok := g.reservations[key]
	if !ok {
		return ErrReservationNotFound
	}
	reservation.State = ReservationCommitted
	g.reservations[key] = reservation
	return nil
}

func (g *Governor) Release(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.reservations[key]; !ok {
		return false
	}
	delete(g.reservations, key)
	return true
}

func (g *Governor) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	reserved := g.reservedLocked()
	snapshot := Snapshot{
		Capacity:  g.capacity,
		Reserved:  reserved,
		Available: g.capacity.SubFloor(reserved),
	}
	for _, reservation := range g.reservations {
		switch reservation.State {
		case ReservationPending:
			snapshot.Pending++
		case ReservationCommitted:
			snapshot.Committed++
		}
	}
	return snapshot
}

func (g *Governor) reservedLocked() workload.Resources {
	var reserved workload.Resources
	for _, reservation := range g.reservations {
		reserved = reserved.Add(reservation.Resources)
	}
	return reserved
}

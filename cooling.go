package profiles

import (
	"context"
	"errors"
	"time"
)

type CoolingControl interface {
	SetCooling(ctx context.Context, owner Principal, closureID string, cancelled bool) error
}

type CoolingIntent struct {
	Owner          Principal
	ClosureID, Key string
	Cancelled      bool
}

type coolingRepository interface {
	PrepareCooling(context.Context, Principal, string, time.Time) (CoolingIntent, error)
	ChangeCooling(context.Context, Principal, string, string, string, time.Time) (Closure, error)
	PendingCooling(context.Context, int) ([]CoolingIntent, error)
	AcknowledgeCooling(context.Context, CoolingIntent) error
}

func (s *Service) applyCooling(ctx context.Context, r coolingRepository, item CoolingIntent) (Closure, error) {
	call, stop := context.WithTimeout(ctx, 3*time.Second)
	err := s.CoolingControl.SetCooling(call, item.Owner, item.ClosureID, item.Cancelled)
	stop()
	if err != nil {
		return Closure{}, ErrUnavailable
	}
	if item.Cancelled {
		if err := r.AcknowledgeCooling(ctx, item); err != nil {
			return Closure{}, err
		}
		return Closure{}, nil
	}
	return r.ChangeCooling(ctx, item.Owner, item.Key, "request", "", s.now())
}

// ReconcileCooling resumes requests interrupted between modules. The repository
// reserves a retry interval so a failed owner cannot starve later requests.
func (s *Service) ReconcileCooling(ctx context.Context, limit int) error {
	if limit < 1 || limit > 100 {
		return ErrInput
	}
	if s.CoolingControl == nil {
		return nil
	}
	r, ok := s.repo.(coolingRepository)
	if !ok {
		return ErrUnavailable
	}
	items, err := r.PendingCooling(ctx, limit)
	if err != nil {
		return err
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		_, e := s.applyCooling(ctx, r, item)
		err = errors.Join(err, e)
	}
	return err
}

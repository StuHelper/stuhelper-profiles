package profiles

import (
	"context"
	"errors"
	"slices"
	"time"
)

var ErrClosed = errors.New("business_account_closed")
var ErrForbidden = errors.New("closure_forbidden")
var ErrAssetsPending = errors.New("closure_assets_pending")

const PointsPolicy = "forfeit_all_points_and_pending_deductions"

// Allow time for the external call and its separate persistence budget.
const closureStepLeaseTTL = 10 * time.Second

var closureSteps = []string{"assets", "sessions", "authorizations", "content"}

type Closure struct {
	ProfileID      string     `json:"profile_id"`
	ID             string     `json:"id"`
	State          string     `json:"state"`
	Revision       int64      `json:"revision"`
	RequestedAt    time.Time  `json:"requested_at"`
	CoolingEndsAt  time.Time  `json:"cooling_ends_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	SkippedModules []string   `json:"skipped_modules,omitempty"`
	PendingReasons []string   `json:"pending_reasons,omitempty"`
	CompletedSteps []string   `json:"completed_steps"`
	Blocker        string     `json:"blocker"`
	PointsPolicy   string     `json:"points_policy"`
}
type ClosureRecord struct {
	Closure Closure
	Owner   Principal
}

// Ports must reconcile an unknown result before retrying a write. Every step is
// permanently idempotent for the closure ID, including after a caller times out.
// Assets atomically freezes future financial writes and certifies settlement.
type ClosureEffects interface {
	ClearAssets(context.Context, Principal, string) error
	RevokeSessions(context.Context, Principal, string) error
	RevokeAuthorizations(context.Context, Principal, string) error
	AnonymizeContent(context.Context, Principal, string) error
}
type ClosureAuthority interface {
	AuthorizeClosure(context.Context, Principal) error
}
type ClosureRepository interface {
	CurrentClosure(context.Context, Principal) (Closure, error)
	Closure(context.Context, string) (ClosureRecord, error)
	Closures(context.Context, string, int) ([]Closure, error)
	ChangeClosure(context.Context, Principal, string, string, string, time.Time) (Closure, error)
	AdvanceClosure(context.Context, string, int, string, []string, time.Time) (Closure, error)
	AccountState(context.Context, Principal) (string, error)
}
type closureStepLeaser interface {
	AcquireClosureStep(context.Context, string, time.Duration) (ClosureRecord, string, error)
	ReleaseClosureStep(context.Context, string, string) error
	AdvanceLeasedClosureStep(context.Context, Principal, string, string, int, string, []string, []string, time.Time) (Closure, error)
}
type closureActorAdvancer interface {
	AdvanceClosureBy(context.Context, Principal, string, int, string, []string, time.Time) (Closure, error)
}

func (s *Service) closureRepository() (ClosureRepository, error) {
	r, ok := s.repo.(ClosureRepository)
	if !ok {
		return nil, ErrUnavailable
	}
	return r, nil
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func validClosureKey(k string) bool { return validIdempotencyKey(k) }
func (s *Service) AccountAccess(ctx context.Context, p Principal) error {
	if !p.Valid() {
		return ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return e
	}
	state, e := r.AccountState(ctx, p)
	if e != nil {
		return e
	}
	if state == "processing" || state == "closed" {
		return ErrClosed
	}
	return nil
}
func (s *Service) OwnClosure(ctx context.Context, p Principal) (Closure, error) {
	if !p.Valid() {
		return Closure{}, ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return Closure{}, e
	}
	return r.CurrentClosure(ctx, p)
}
func (s *Service) RequestClosure(ctx context.Context, p Principal, key string, acknowledged bool) (Closure, error) {
	if !p.Valid() || !validClosureKey(key) || !acknowledged {
		return Closure{}, ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return Closure{}, e
	}
	return r.ChangeClosure(ctx, p, key, "request", "", s.now())
}
func (s *Service) CancelClosure(ctx context.Context, p Principal, key, id string) (Closure, error) {
	if !p.Valid() || !validClosureKey(key) || !validUUID(id) {
		return Closure{}, ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return Closure{}, e
	}
	return r.ChangeClosure(ctx, p, key, "cancel", id, s.now())
}
func (s *Service) authorizeClosure(ctx context.Context, p Principal) error {
	if !p.Valid() || s.ClosureAuthority == nil {
		return ErrForbidden
	}
	if e := s.ClosureAuthority.AuthorizeClosure(ctx, p); e != nil {
		return e
	}
	return nil
}
func (s *Service) AdminClosure(ctx context.Context, p Principal, id string) (Closure, error) {
	if e := s.authorizeClosure(ctx, p); e != nil {
		return Closure{}, e
	}
	if !validUUID(id) {
		return Closure{}, ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return Closure{}, e
	}
	v, e := r.Closure(ctx, id)
	return v.Closure, e
}
func (s *Service) ListClosures(ctx context.Context, p Principal, after string, limit int) ([]Closure, error) {
	if e := s.authorizeClosure(ctx, p); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 100 || (after != "" && !validUUID(after)) {
		return nil, ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return nil, e
	}
	return r.Closures(ctx, after, limit)
}

func (s *Service) acquireClosureTurn(ctx context.Context, id string) (func(), bool, error) {
	waited := false
	for {
		s.closureMu.Lock()
		if s.closureTurns == nil {
			s.closureTurns = map[string]chan struct{}{}
		}
		ch, busy := s.closureTurns[id]
		if !busy {
			done := make(chan struct{})
			s.closureTurns[id] = done
			s.closureMu.Unlock()
			return func() {
				s.closureMu.Lock()
				delete(s.closureTurns, id)
				close(done)
				s.closureMu.Unlock()
			}, waited, nil
		}
		s.closureMu.Unlock()
		waited = true
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}
}

// ProcessClosure resumes durable steps. A dependency outage returns explicit
// progress and never promotes an incomplete operation to closed.
func (s *Service) ProcessClosure(ctx context.Context, p Principal, key, id string) (Closure, error) {
	if e := s.authorizeClosure(ctx, p); e != nil {
		return Closure{}, e
	}
	return s.processClosure(ctx, p, key, id, false)
}

func (s *Service) processClosure(ctx context.Context, p Principal, key, id string, automatic bool) (Closure, error) {
	if !validClosureKey(key) || !validUUID(id) {
		return Closure{}, ErrInput
	}
	r, e := s.closureRepository()
	if e != nil {
		return Closure{}, e
	}
	v, e := r.ChangeClosure(ctx, p, key, "approve", id, s.now())
	if e != nil || v.State == "closed" {
		return v, e
	}
	initialRevision := v.Revision
	releaseTurn, waitedLocal, e := s.acquireClosureTurn(ctx, id)
	if e != nil {
		return v, e
	}
	defer releaseTurn()

	record, e := r.Closure(ctx, id)
	if e != nil {
		return v, e
	}
	v = record.Closure
	if v.State == "closed" || ((automatic || (waitedLocal && v.Revision != initialRevision)) && v.Blocker != "") {
		return v, nil
	}
	leaser, hasLeaser := r.(closureStepLeaser)
	for step := len(v.CompletedSteps); step < len(closureSteps) && v.State != "closed"; step = len(v.CompletedSteps) {
		if ctx.Err() != nil {
			return v, ctx.Err()
		}
		var lease string
		if hasLeaser {
			waitedPeer := false
			for {
				rec, token, leaseErr := leaser.AcquireClosureStep(ctx, id, closureStepLeaseTTL)
				if leaseErr != nil {
					return v, leaseErr
				}
				record = rec
				v = rec.Closure
				if token != "" {
					lease = token
					if (automatic || (waitedPeer && v.Revision != initialRevision)) && v.Blocker != "" {
						relCtx, cancelRel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
						_ = leaser.ReleaseClosureStep(relCtx, id, lease)
						cancelRel()
						return v, nil
					}
					break
				}
				if v.State == "closed" {
					return v, nil
				}
				if v.State != "awaiting_assets" && v.State != "processing" {
					return v, ErrConflict
				}
				waitedPeer = true
				select {
				case <-time.After(20 * time.Millisecond):
				case <-ctx.Done():
					return v, ctx.Err()
				}
			}
			step = len(v.CompletedSteps)
			if step >= len(closureSteps) || v.State == "closed" {
				relCtx, cancelRel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				_ = leaser.ReleaseClosureStep(relCtx, id, lease)
				cancelRel()
				return v, nil
			}
		}

		name := closureSteps[step]
		var skipped []string
		var pendingReasons []string
		err := ErrUnavailable
		if s.ClosureEffects != nil {
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			switch name {
			case "assets":
				if detail, ok := s.ClosureEffects.(AssetsClearanceEffects); ok {
					var clearance AssetsClearance
					clearance, err = detail.ClearAssetsDetail(call, record.Owner, id)
					skipped = clearance.SkippedModules
					pendingReasons = clearance.PendingReasons
				} else if effects, ok := s.ClosureEffects.(interface {
					ClearAssetsResult(context.Context, Principal, string) ([]string, error)
				}); ok {
					skipped, err = effects.ClearAssetsResult(call, record.Owner, id)
				} else {
					err = s.ClosureEffects.ClearAssets(call, record.Owner, id)
				}
			case "sessions":
				err = s.ClosureEffects.RevokeSessions(call, record.Owner, id)
			case "authorizations":
				if effects, ok := s.ClosureEffects.(interface {
					RevokeAuthorizationsResult(context.Context, Principal, string) ([]string, error)
				}); ok {
					skipped, err = effects.RevokeAuthorizationsResult(call, record.Owner, id)
				} else {
					err = s.ClosureEffects.RevokeAuthorizations(call, record.Owner, id)
				}
			case "content":
				if effects, ok := s.ClosureEffects.(interface {
					AnonymizeContentResult(context.Context, Principal, string) ([]string, error)
				}); ok {
					skipped, err = effects.AnonymizeContentResult(call, record.Owner, id)
				} else {
					err = s.ClosureEffects.AnonymizeContent(call, record.Owner, id)
				}
				if err == nil {
					err = s.releaseClosureAvatars(call, record.Owner)
				}
			}
			cancel()
		}
		// Stopping the worker is not a dependency failure. Leave an uncertain
		// step eligible for a new process to reconcile with the same closure ID.
		if automatic && err != nil && ctx.Err() != nil {
			if hasLeaser {
				relCtx, cancelRel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				_ = leaser.ReleaseClosureStep(relCtx, id, lease)
				cancelRel()
			}
			return v, ctx.Err()
		}
		blocker := ""
		if err != nil {
			blocker = name + "_unavailable"
			if name == "assets" && errors.Is(err, ErrAssetsPending) {
				blocker = "assets_pending"
				pendingReasons = extractPendingReasons(pendingReasons, err)
			} else {
				pendingReasons = nil
			}
		} else {
			pendingReasons = nil
		}
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		if hasLeaser {
			v, e = leaser.AdvanceLeasedClosureStep(persistCtx, p, id, lease, step, blocker, pendingReasons, skipped, s.now())
		} else if rr, ok := r.(closureReasonsRepository); ok {
			v, e = rr.AdvanceClosureByWithReasons(persistCtx, p, id, step, blocker, pendingReasons, skipped, s.now())
		} else if advancer, ok := r.(closureActorAdvancer); ok {
			v, e = advancer.AdvanceClosureBy(persistCtx, p, id, step, blocker, skipped, s.now())
		} else {
			v, e = r.AdvanceClosure(persistCtx, id, step, blocker, skipped, s.now())
		}
		if e != nil && hasLeaser {
			_ = leaser.ReleaseClosureStep(persistCtx, id, lease)
		}
		cancelPersist()
		if e != nil {
			return v, e
		}
		if v.State == "closed" {
			return v, nil
		}
		if ctx.Err() != nil {
			return v, ctx.Err()
		}
		if v.Blocker != "" {
			return v, nil
		}
	}
	return v, nil
}
func ValidClosureProgress(v Closure) bool {
	if len(v.CompletedSteps) > len(closureSteps) || !slices.Equal(v.CompletedSteps, closureSteps[:len(v.CompletedSteps)]) {
		return false
	}
	if len(v.PendingReasons) > 0 {
		normalized, ok := NormalizePendingReasons(v.PendingReasons)
		if !ok || v.Blocker != "assets_pending" || len(normalized) != len(v.PendingReasons) {
			return false
		}
	}
	return v.PointsPolicy == PointsPolicy && v.CoolingEndsAt.Equal(v.RequestedAt.Add(7*24*time.Hour)) && (v.State != "closed" || len(v.CompletedSteps) == 4)
}

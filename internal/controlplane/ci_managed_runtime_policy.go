package controlplane

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type CIManagedMode string

const (
	CIManagedOff     CIManagedMode = "off"
	CIManagedRecover CIManagedMode = "recover"
	CIManagedRun     CIManagedMode = "run"
)

// CIManagedRuntimeConfig configures execution, never infrastructure funding.
// Recover mode cannot create runners or serve guest bootstrap/admission.
type CIManagedRuntimeConfig struct {
	Mode          CIManagedMode
	ProfileJSON   []byte
	PublicURL     string
	SigningKey    []byte
	EnforcerURL   string
	EnforcerToken []byte
	HTTPClient    *http.Client
}

const ciManagedFundingPoolSQL = `
SELECT profile_digest,authorized_micros,committed_micros,reservation_micros,
       max_concurrent,admit_before,
       (SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL)
FROM ci_provisioning_budget
WHERE singleton`

// This is an atomic read-only readiness snapshot, not a reservation or a grant.
// claimCIManagedRunner remains the authoritative transactional admission gate.
func (s *Store) ciManagedFundingPool(ctx context.Context, profileDigest string) (CIPool, error) {
	var profile string
	var authorized, committed, reservation, active int64
	var maximum int
	var admitBefore time.Time
	err := s.pool.QueryRow(ctx, ciManagedFundingPoolSQL).Scan(
		&profile, &authorized, &committed, &reservation, &maximum, &admitBefore, &active,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CIPool{Status: "unfunded", Message: "Managed CI requires an explicit infrastructure funding authorization."}, nil
	}
	if err != nil {
		return CIPool{}, err
	}
	if reservation <= 0 || maximum < 1 || maximum > 1000 || committed < 0 ||
		authorized < committed || active < 0 || admitBefore.Year() < 1970 || admitBefore.Year() > 9999 {
		return CIPool{}, ErrConflict
	}
	if profile != profileDigest {
		return CIPool{Status: "profile_mismatch", Message: "The infrastructure authorization does not match the configured runner profile."}, nil
	}
	if admitBefore.Before(s.now().Add(ciLifecycleSetupWindow)) {
		return CIPool{Status: "funding_expired", Message: "The infrastructure authorization cannot admit another runner within its setup deadline."}, nil
	}
	if authorized-committed < reservation {
		return CIPool{Status: "funding_exhausted", Message: "The infrastructure authorization has no remaining runner allocation."}, nil
	}
	if active >= int64(maximum) {
		return CIPool{Status: "at_capacity", Message: "Authorized runner capacity is occupied; verified cleanup must release it."}, nil
	}
	return CIPool{
		Ready:   true,
		Status:  "ready",
		Message: "Managed scheduling is running within an explicit infrastructure envelope; repository and workspace limits still apply.",
	}, nil
}

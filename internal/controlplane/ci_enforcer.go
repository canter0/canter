package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/pricing"
)

// ciEnforcerRunnerName is the canonical GitHub runner identity: the prefix
// plus the first 48 lowercase hex characters of SHA256(operationID).
func ciEnforcerRunnerName(operationID string) string {
	sum := sha256Sum([]byte(operationID))
	return "canter-ci-" + hex.EncodeToString(sum[:])[:48]
}

func sha256Sum(b []byte) [32]byte {
	return sha256.Sum256(b)
}

// normalizeMillis is the enforcer's pinned UTC-millisecond plan normalization
// used for digest and comparison; it never mutates the microsecond-pinned
// lifecycle plan used by the database.
func (p ciRunnerLifecyclePlan) normalizeMillis() ciRunnerLifecyclePlan {
	p.SetupDeadlineAt = p.SetupDeadlineAt.UTC().Truncate(time.Millisecond)
	p.HardDeadlineAt = p.HardDeadlineAt.UTC().Truncate(time.Millisecond)
	return p
}

// ciEnforcerNewLeaseID generates a random 64-hex lease identity for the
// parent to pin into plan.EnforcerLease. The enforcer never derives lease
// identity from the operation.
func ciEnforcerNewLeaseID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ciEnforcerSpec is the pinned nonsecret lease allocation. It carries no
// tokens, JIT configuration, userdata, or credentials.
type ciEnforcerSpec struct {
	Plan                 ciRunnerLifecyclePlan `json:"plan"`
	GitHubInstallationID int64                 `json:"githubInstallationId"`
	GitHubRepositoryID   int64                 `json:"githubRepositoryId"`
	SizeID               string                `json:"sizeId"`
}

func (s ciEnforcerSpec) normalize() ciEnforcerSpec {
	s.Plan = s.Plan.normalizeMillis()
	return s
}

func (s ciEnforcerSpec) valid() bool {
	s = s.normalize()
	plan := s.Plan
	if !plan.shapeValid() || !ciLifecycleLeaseID.MatchString(plan.EnforcerLease) {
		return false
	}
	if plan.ServerName != ciEnforcerRunnerName(plan.OperationID) {
		return false
	}
	if _, ok := pricing.FindCI(s.SizeID); !ok {
		return false
	}
	return s.GitHubInstallationID > 0 && s.GitHubRepositoryID > 0 &&
		ciLifecycleID.MatchString(s.SizeID)
}

func (s ciEnforcerSpec) digests() (planDigest, specDigest string) {
	s = s.normalize()
	raw, _ := json.Marshal(s.Plan)
	sum := sha256.Sum256(raw)
	planDigest = hex.EncodeToString(sum[:])
	raw, _ = json.Marshal(s)
	sum = sha256.Sum256(raw)
	specDigest = hex.EncodeToString(sum[:])
	return planDigest, specDigest
}

// ciEnforcerLease is the durable per-lease journal record. It never stores
// tokens, raw JIT configuration, userdata, or HTTP bodies.
type ciEnforcerLease struct {
	LeaseID                string                     `json:"leaseId"`
	Spec                   ciEnforcerSpec             `json:"spec"`
	PlanDigest             string                     `json:"planDigest"`
	SpecDigest             string                     `json:"specDigest"`
	DeadlineUnixMs         int64                      `json:"deadlineUnixMs"`
	ApprovedDeadlineUnixMs int64                      `json:"approvedDeadlineUnixMs,omitempty"`
	CreateSettled          bool                       `json:"createSettled"`
	CreateResult           string                     `json:"createResult,omitempty"`
	CreateServerID         string                     `json:"createServerId,omitempty"`
	Resources              []compute.CIRunnerResource `json:"resources,omitempty"`
	RegistrationAttempted  bool                       `json:"registrationAttempted"`
	RegistrationSettled    bool                       `json:"registrationSettled"`
	RegistrationConflict   bool                       `json:"registrationConflict,omitempty"`
	GitHubRunnerID         int64                      `json:"githubRunnerId,omitempty"`
	PhysicalAbsent         bool                       `json:"physicalAbsent"`
	RegAbsent              bool                       `json:"regAbsent"`
	StopRequested          bool                       `json:"stopRequested"`
	Settled                bool                       `json:"settled"`

	// cutoffMono is the in-process monotonic cutoff paired with
	// DeadlineUnixMs; the earlier of the two ends the lease. It is rebuilt
	// from the remaining absolute duration on restart and never persisted.
	cutoffMono time.Time `json:"-"`
}

// ciEnforcerLeaseState owns one lease's mutable runtime: the committed
// snapshot plus per-lease serialization. stateMu guards the snapshot and
// availability latch; physicalMu serializes provider passes; registering
// fences settlement while a JIT attempt is in flight.
type ciEnforcerLeaseState struct {
	stateMu     sync.Mutex
	lease       *ciEnforcerLease
	physicalMu  sync.Mutex
	registering bool
	unavailable bool
}

func (s *ciEnforcerLeaseState) snapshot() *ciEnforcerLease {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return cloneLease(s.lease)
}

func (s *ciEnforcerLeaseState) isUnavailable() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.unavailable
}

func cloneLease(l *ciEnforcerLease) *ciEnforcerLease {
	if l == nil {
		return nil
	}
	out := *l
	out.Resources = append([]compute.CIRunnerResource(nil), l.Resources...)
	out.Spec.Plan = l.Spec.Plan.normalizeMillis()
	return &out
}

// ciEnforcerCompute is the narrow provider seam; *compute.Client satisfies it
// and tests may substitute fixtures.
type ciEnforcerCompute interface {
	ValidateCIRunnerScope(scope compute.CIRunnerScope) error
	ObserveCIRunner(ctx context.Context, scope compute.CIRunnerScope, retained []compute.CIRunnerResource) (compute.CIRunnerObservation, error)
	StopCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error
	DeleteCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error
	Server(ctx context.Context, id string) (compute.Server, error)
}

var (
	errCIEnforcerUnavailable    = errors.New("CI enforcer is unavailable")
	errCIEnforcerConflict       = errors.New("CI enforcer lease conflict")
	errCIEnforcerRunnerConflict = fmt.Errorf("%w: CI runner registration conflict", ErrConflict)
)

const (
	ciEnforcerMaxLeases   = 1000
	ciEnforcerStateMax    = 256 << 10
	ciEnforcerLeaseIDSize = 64
)

var ciLifecycleLeaseID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ciEnforcerEngine struct {
	dir      string
	compute  ciEnforcerCompute
	github   *ciGitHubClient
	wallNow  func() time.Time
	monoNow  func() time.Time
	mu       sync.Mutex
	leases   map[string]*ciEnforcerLeaseState
	opIndex  map[string]string
	flock    *os.File
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
	running  bool
	runWg    sync.WaitGroup
	inflight sync.WaitGroup
}

func ciEnforcerScope(spec ciEnforcerSpec) compute.CIRunnerScope {
	return compute.CIRunnerScope{
		OperationID: spec.Plan.OperationID,
		ServerName:  spec.Plan.ServerName,
		Region:      spec.Plan.Region,
		NetworkID:   spec.Plan.NetworkID,
	}
}

// freezeCutoff records the absolute Unix deadline plus an in-process
// monotonic cutoff derived from the monotonic clock; the earlier of the two
// ends the lease. On restart the remaining monotonic duration is rebuilt
// from the original absolute cutoff, never a fresh window.
func (l *ciEnforcerLease) freezeCutoff(deadlineUnixMs int64, wall, mono time.Time) {
	l.DeadlineUnixMs = deadlineUnixMs
	remaining := time.Duration(deadlineUnixMs-wall.UnixMilli()) * time.Millisecond
	if remaining < 0 {
		remaining = 0
	}
	l.cutoffMono = mono.Add(remaining)
}

func (l *ciEnforcerLease) expired(wall, mono time.Time) bool {
	if wall.UnixMilli() >= l.DeadlineUnixMs {
		return true
	}
	return !mono.Before(l.cutoffMono)
}

func (e *ciEnforcerEngine) now() (wall, mono time.Time) {
	return e.wallNow(), e.monoNow()
}

// Register work before releasing the shutdown gate; Close cannot release
// the process lock while an accepted operation can still write state.
func (e *ciEnforcerEngine) beginWork(ctx context.Context) (context.Context, func(), error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, errCIEnforcerUnavailable
	}
	e.inflight.Add(1)
	e.mu.Unlock()
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	return work, func() {
		stop()
		cancel()
		e.inflight.Done()
	}, nil
}

func (e *ciEnforcerEngine) leasePath(leaseID string) string {
	return filepath.Join(e.dir, "lease-"+leaseID+".json")
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// persistLease atomically replaces the lease file: temp 0600, fsync, rename,
// directory fsync. The caller must invoke it BEFORE publishing state.
func (e *ciEnforcerEngine) persistLease(l *ciEnforcerLease) error {
	path := e.leasePath(l.LeaseID)
	tmp, err := os.CreateTemp(e.dir, ".lease-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = os.Chmod(tmpName, 0o600); err != nil {
		tmp.Close()
		return err
	}
	raw, err := json.Marshal(l)
	if err != nil {
		tmp.Close()
		return err
	}
	if len(raw) > ciEnforcerStateMax {
		tmp.Close()
		return errors.New("enforcer lease state too large")
	}
	if _, err = tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(e.dir)
}

// ciStrictJSONValue walks one JSON value rejecting duplicate object keys,
// null literals, and malformed structure at every nesting level.
func ciStrictJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case nil:
		return errors.New("null value")
	case json.Delim:
		switch t {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok || seen[key] {
					return errors.New("duplicate or invalid key")
				}
				seen[key] = true
				if err := ciStrictJSONValue(dec); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := ciStrictJSONValue(dec); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		}
		return errors.New("unexpected delimiter")
	default:
		return nil
	}
}

// ciEnforcerStrictJSON validates the entire document strictly (object root,
// unique keys, no nulls, no trailing data) then decodes with unknown-field
// rejection.
func ciEnforcerStrictJSON(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("expected JSON object")
	}
	// Re-decode the object body strictly: rewind by wrapping — simpler to
	// validate the whole stream token-wise from the start.
	dec = json.NewDecoder(bytes.NewReader(raw))
	if err := ciStrictJSONValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	dec2 := json.NewDecoder(bytes.NewReader(raw))
	dec2.DisallowUnknownFields()
	return dec2.Decode(out)
}

// loadLeaseFile reads and strictly validates one durable record through an
// O_NOFOLLOW descriptor: Fstat must show a regular, owner-matching, mode
// 0600, single-link file within the size bound before any byte is trusted.
// Corrupt, oversized, non-regular, wrong-owner or wrong-mode state refuses
// startup; nothing is silently skipped.
func (e *ciEnforcerEngine) loadLeaseFile(leaseID string) (*ciEnforcerLease, error) {
	path := e.leasePath(leaseID)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("enforcer lease state is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, errors.New("enforcer lease state has wrong permissions")
	}
	if info.Size() > ciEnforcerStateMax {
		return nil, errors.New("enforcer lease state oversized")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return nil, errors.New("enforcer lease state has wrong owner or link count")
	}
	raw, err := io.ReadAll(io.LimitReader(f, ciEnforcerStateMax+1))
	if err != nil || int64(len(raw)) > ciEnforcerStateMax {
		return nil, errors.New("enforcer lease state unreadable or oversized")
	}
	var l ciEnforcerLease
	if err = ciEnforcerStrictJSON(raw, &l); err != nil {
		return nil, errors.New("enforcer lease state corrupt")
	}
	if l.LeaseID != leaseID || !ciLifecycleLeaseID.MatchString(l.LeaseID) {
		return nil, errors.New("enforcer lease state corrupt")
	}
	seenRes := map[string]bool{}
	for _, r := range l.Resources {
		key := r.Kind + "/" + r.ID
		if (r.Kind != "server" && r.Kind != "port" && r.Kind != "security_group") ||
			!ciLifecycleID.MatchString(r.ID) || r.OperationID != l.Spec.Plan.OperationID || seenRes[key] {
			return nil, errors.New("enforcer lease state corrupt")
		}
		seenRes[key] = true
	}
	if l.Settled {
		// Tombstone: only identity/digests/flags survive; no spec or IDs.
		if l.Spec.Plan.OperationID == "" || l.Spec.Plan.EnforcerLease != l.LeaseID ||
			l.Spec.GitHubRepositoryID != 0 || l.Spec.GitHubInstallationID != 0 ||
			l.Spec.SizeID != "" || len(l.Resources) != 0 || l.GitHubRunnerID != 0 ||
			l.CreateServerID != "" || l.CreateSettled || l.RegistrationSettled ||
			l.RegistrationAttempted || l.CreateResult != "" || l.DeadlineUnixMs != 0 {
			return nil, errors.New("enforcer lease state corrupt")
		}
		wantPlan := ciRunnerLifecyclePlan{OperationID: l.Spec.Plan.OperationID, EnforcerLease: leaseID}
		if !ciLifecycleID.MatchString(l.Spec.Plan.OperationID) ||
			l.Spec.Plan.normalizeMillis() != wantPlan ||
			!l.StopRequested || !l.PhysicalAbsent || !l.RegAbsent ||
			l.ApprovedDeadlineUnixMs != 0 || l.RegistrationConflict ||
			!ciLifecycleLeaseID.MatchString(l.PlanDigest) || !ciLifecycleLeaseID.MatchString(l.SpecDigest) {
			return nil, errors.New("enforcer lease state corrupt")
		}
	} else {
		if l.DeadlineUnixMs <= 0 ||
			(l.ApprovedDeadlineUnixMs != 0 && l.ApprovedDeadlineUnixMs != l.DeadlineUnixMs) {
			return nil, errors.New("enforcer lease state corrupt")
		}
		pd, sd := l.Spec.digests()
		if pd != l.PlanDigest || sd != l.SpecDigest {
			return nil, errors.New("enforcer lease state corrupt")
		}
		if !l.Spec.valid() {
			return nil, errors.New("enforcer lease state corrupt")
		}
		if l.Spec.Plan.EnforcerLease != l.LeaseID {
			return nil, errors.New("enforcer lease state corrupt")
		}
		if l.DeadlineUnixMs > l.Spec.Plan.HardDeadlineAt.UnixMilli() ||
			(l.StopRequested && l.ApprovedDeadlineUnixMs != 0) ||
			(!l.StopRequested && l.ApprovedDeadlineUnixMs == 0 &&
				l.DeadlineUnixMs != l.Spec.Plan.SetupDeadlineAt.UnixMilli()) {
			return nil, errors.New("enforcer lease state corrupt")
		}
		if l.CreateSettled {
			if l.CreateResult != "created" && l.CreateResult != "rejected" {
				return nil, errors.New("enforcer lease state corrupt")
			}
			if l.CreateResult == "created" && (l.CreateServerID == "" || !seenRes["server/"+l.CreateServerID]) {
				return nil, errors.New("enforcer lease state corrupt")
			}
			if l.CreateResult == "rejected" && l.CreateServerID != "" {
				return nil, errors.New("enforcer lease state corrupt")
			}
		} else if l.CreateResult != "" || l.CreateServerID != "" {
			return nil, errors.New("enforcer lease state corrupt")
		}
		if !l.RegistrationAttempted && l.RegistrationSettled {
			return nil, errors.New("enforcer lease state corrupt")
		}
		if l.GitHubRunnerID < 0 ||
			(l.GitHubRunnerID > 0 && (!l.RegistrationAttempted || !l.RegistrationSettled || l.RegAbsent || l.RegistrationConflict)) ||
			(l.RegistrationAttempted && (!l.CreateSettled || l.CreateResult != "created")) ||
			((l.PhysicalAbsent || l.RegAbsent) && !l.StopRequested) {
			return nil, errors.New("enforcer lease state corrupt")
		}
	}
	// Rebuild the monotonic cutoff's remaining duration from the original
	// absolute deadline; never renew the window on restart.
	wall, mono := e.now()
	l.cutoffMono = mono.Add(time.Duration(l.DeadlineUnixMs-wall.UnixMilli()) * time.Millisecond)
	if l.cutoffMono.Before(mono) {
		l.cutoffMono = mono
	}
	return &l, nil
}

func (e *ciEnforcerEngine) stateFor(leaseID string) (*ciEnforcerLeaseState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errCIEnforcerUnavailable
	}
	return e.leases[leaseID], nil
}

// commit applies a copy-on-write transition: mutate the copy, persist it,
// then publish. A persistence failure latches the lease unavailable so no
// approval, registration, or settled acknowledgement can be claimed.
func (e *ciEnforcerEngine) commit(st *ciEnforcerLeaseState, mutate func(*ciEnforcerLease)) error {
	st.stateMu.Lock()
	defer st.stateMu.Unlock()
	if st.unavailable {
		return errCIEnforcerUnavailable
	}
	next := cloneLease(st.lease)
	mutate(next)
	if err := e.persistLease(next); err != nil {
		st.unavailable = true
		return err
	}
	st.lease = next
	return nil
}

func (e *ciEnforcerEngine) statusOf(st *ciEnforcerLeaseState) ciEnforcerStatus {
	l := st.snapshot()
	return leaseStatus(l)
}

func leaseStatus(l *ciEnforcerLease) ciEnforcerStatus {
	s := ciEnforcerStatus{
		LeaseID: l.LeaseID, OperationID: l.Spec.Plan.OperationID,
		PlanDigest: l.PlanDigest, SpecDigest: l.SpecDigest,
		DeadlineUnixMs: l.DeadlineUnixMs,
		CreateSettled:  l.CreateSettled, RegistrationAttempted: l.RegistrationAttempted,
		RegistrationSettled: l.RegistrationSettled, GitHubRunnerID: l.GitHubRunnerID,
		GitHubRepositoryID: l.Spec.GitHubRepositoryID,
		RunnerName:         ciEnforcerRunnerName(l.Spec.Plan.OperationID),
		StopRequested:      l.StopRequested, Settled: l.Settled,
		Resources: []ciOwnedResource{},
	}
	if !l.Settled {
		for _, r := range l.Resources {
			s.Resources = append(s.Resources, ciOwnedResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
		}
	} else {
		// Tombstone: private metadata, deadlines and inventory are erased;
		// operation identity, digests and stop/settled flags remain. The
		// status reports both independent proofs TRUE so downstream evidence
		// can finish; the durable record keeps them false.
		s.GitHubRepositoryID = 0
		s.GitHubRunnerID = 0
		s.RunnerName = ""
		s.DeadlineUnixMs = 0
		s.CreateSettled = true
		s.RegistrationSettled = true
		s.RegistrationAttempted = false
	}
	return s
}

func (e *ciEnforcerEngine) status(leaseID string) (ciEnforcerStatus, error) {
	st, err := e.stateFor(leaseID)
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	if st == nil {
		return ciEnforcerStatus{}, nil
	}
	if st.isUnavailable() {
		return ciEnforcerStatus{}, errCIEnforcerUnavailable
	}
	return e.statusOf(st), nil
}

// putLease persists a pinned spec before acknowledging. The URL lease ID
// must equal plan.EnforcerLease; an operation may have only one lease,
// including tombstoned ones. The whole check-persist-publish section runs
// under the registry lock (filesystem I/O only, never HTTP) so concurrent
// PUTs cannot both admit different specs. Exact retries are idempotent even
// after expiry.
func (e *ciEnforcerEngine) putLease(leaseID string, spec ciEnforcerSpec) (ciEnforcerStatus, error) {
	spec = spec.normalize()
	planDigest, specDigest := spec.digests()
	if !ciLifecycleLeaseID.MatchString(leaseID) || leaseID != spec.Plan.EnforcerLease || !spec.valid() {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if err := e.compute.ValidateCIRunnerScope(ciEnforcerScope(spec)); err != nil {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ciEnforcerStatus{}, errCIEnforcerUnavailable
	}
	if owner, exists := e.opIndex[spec.Plan.OperationID]; exists && owner != leaseID {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if st, exists := e.leases[leaseID]; exists {
		l := st.snapshot()
		if l == nil || l.Settled || l.SpecDigest != specDigest || l.PlanDigest != planDigest {
			return ciEnforcerStatus{}, errCIEnforcerConflict
		}
		if st.isUnavailable() {
			return ciEnforcerStatus{}, errCIEnforcerUnavailable
		}
		return leaseStatus(l), nil
	}
	wall, mono := e.now()
	if !spec.Plan.newPlanValid(wall) {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	count, err := e.leaseRecordCount()
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	if count >= ciEnforcerMaxLeases {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	l := &ciEnforcerLease{
		LeaseID: leaseID, Spec: spec, PlanDigest: planDigest, SpecDigest: specDigest,
		CreateResult: "", Resources: []compute.CIRunnerResource{},
	}
	l.freezeCutoff(spec.Plan.SetupDeadlineAt.UnixMilli(), wall, mono)
	if err = e.persistLease(l); err != nil {
		return ciEnforcerStatus{}, err
	}
	e.leases[leaseID] = &ciEnforcerLeaseState{lease: l}
	e.opIndex[spec.Plan.OperationID] = leaseID
	return leaseStatus(l), nil
}

// rejectUnstarted durably records that creation never fenced a provider
// stage: the caller has already committed the control-plane stop and the
// unstarted-creation seal, so this STOP-ONLY transition stays valid after
// the setup or hard deadline. A missing lease is allocated directly in the
// rejected state; an existing lease must still be pending creation or
// already creation-rejected with no registration, approval, or server
// identity. It never fakes provider or GitHub absence, never releases
// funding, extends no deadline, and performs no provider or GitHub I/O.
func (e *ciEnforcerEngine) rejectUnstarted(leaseID string, spec ciEnforcerSpec) (ciEnforcerStatus, error) {
	spec = spec.normalize()
	planDigest, specDigest := spec.digests()
	setupUnixMs := spec.Plan.SetupDeadlineAt.UnixMilli()
	hardUnixMs := spec.Plan.HardDeadlineAt.UnixMilli()
	if !ciLifecycleLeaseID.MatchString(leaseID) || leaseID != spec.Plan.EnforcerLease ||
		!spec.valid() || setupUnixMs <= 0 || hardUnixMs <= setupUnixMs {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if err := e.compute.ValidateCIRunnerScope(ciEnforcerScope(spec)); err != nil {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ciEnforcerStatus{}, errCIEnforcerUnavailable
	}
	if owner, exists := e.opIndex[spec.Plan.OperationID]; exists && owner != leaseID {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if st, exists := e.leases[leaseID]; exists {
		return e.commitGuarded(st, func(next *ciEnforcerLease) error {
			if next.PlanDigest != planDigest || next.SpecDigest != specDigest {
				return errCIEnforcerConflict
			}
			if next.Settled {
				// Exact tombstone retry: never revive or rewrite.
				return errCIEnforcerRetry
			}
			if st.registering || next.RegistrationAttempted || next.RegistrationConflict ||
				next.GitHubRunnerID > 0 || next.ApprovedDeadlineUnixMs != 0 ||
				next.CreateServerID != "" || next.CreateResult == "created" {
				return errCIEnforcerConflict
			}
			for _, r := range next.Resources {
				if r.Kind == "server" {
					return errCIEnforcerConflict
				}
			}
			if next.StopRequested && next.CreateSettled && next.CreateResult == "rejected" {
				// Already durably rejected: preserve accumulated absence
				// proofs and the original deadline.
				return errCIEnforcerRetry
			}
			if next.CreateSettled && next.CreateResult != "rejected" {
				return errCIEnforcerConflict
			}
			return nil
		}, func(next *ciEnforcerLease) {
			next.StopRequested = true
			next.ApprovedDeadlineUnixMs = 0
			next.CreateSettled = true
			next.CreateResult = "rejected"
			next.CreateServerID = ""
			next.PhysicalAbsent = false
			next.RegAbsent = false
		})
	}
	wall, mono := e.now()
	count, err := e.leaseRecordCount()
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	if count >= ciEnforcerMaxLeases {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	l := &ciEnforcerLease{
		LeaseID: leaseID, Spec: spec, PlanDigest: planDigest, SpecDigest: specDigest,
		StopRequested: true, CreateSettled: true, CreateResult: "rejected",
		Resources: []compute.CIRunnerResource{},
	}
	l.freezeCutoff(setupUnixMs, wall, mono)
	if err = e.persistLease(l); err != nil {
		return ciEnforcerStatus{}, err
	}
	e.leases[leaseID] = &ciEnforcerLeaseState{lease: l}
	e.opIndex[spec.Plan.OperationID] = leaseID
	return leaseStatus(l), nil
}

func (e *ciEnforcerEngine) leaseRecordCount() (int, error) {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, "lease-") && strings.HasSuffix(name, ".json") {
			count++
		}
	}
	return count, nil
}

// approve performs the once-only cutoff replacement. The committed receipt
// deadline may shorten or extend the current cutoff but must be strictly in
// the future, never beyond the hard deadline or now+ciMaxRuntimeSeconds.
// Preconditions: creation closed with an owned server and a settled,
// positively-identified GitHub registration.
func (e *ciEnforcerEngine) approve(leaseID string, executionDeadlineUnixMs int64) (ciEnforcerStatus, error) {
	_, done, err := e.beginWork(context.Background())
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	defer done()
	st, err := e.stateFor(leaseID)
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	if st == nil {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	var wall, mono time.Time
	return e.commitGuarded(st, func(next *ciEnforcerLease) error {
		wall, mono = e.now()
		if next.Settled || next.StopRequested {
			return errCIEnforcerConflict
		}
		if next.expired(wall, mono) || executionDeadlineUnixMs <= wall.UnixMilli() {
			return errCIEnforcerConflict
		}
		hard := next.Spec.Plan.HardDeadlineAt.UnixMilli()
		maxAhead := wall.Add(time.Duration(ciMaxRuntimeSeconds) * time.Second).UnixMilli()
		if executionDeadlineUnixMs > hard || executionDeadlineUnixMs > maxAhead {
			return errCIEnforcerConflict
		}
		// Readiness: a source-admitted runner must have a created owned VM
		// and a settled, positively-identified GitHub registration.
		if !next.CreateSettled || next.CreateResult != "created" || next.CreateServerID == "" ||
			!next.RegistrationSettled || next.GitHubRunnerID <= 0 {
			return errCIEnforcerConflict
		}
		if next.ApprovedDeadlineUnixMs != 0 {
			if next.ApprovedDeadlineUnixMs == executionDeadlineUnixMs {
				return errCIEnforcerRetry
			}
			return errCIEnforcerConflict
		}
		return nil
	}, func(next *ciEnforcerLease) {
		next.ApprovedDeadlineUnixMs = executionDeadlineUnixMs
		next.freezeCutoff(executionDeadlineUnixMs, wall, mono)
	})
}

// stop records the durable irrevocable stop intent before any cleanup.
func (e *ciEnforcerEngine) stop(leaseID string) (ciEnforcerStatus, error) {
	_, done, err := e.beginWork(context.Background())
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	defer done()
	st, err := e.stateFor(leaseID)
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	if st == nil {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	return e.commitGuarded(st, func(next *ciEnforcerLease) error {
		if next.StopRequested {
			return errCIEnforcerRetry
		}
		return nil
	}, func(next *ciEnforcerLease) {
		next.StopRequested = true
		next.ApprovedDeadlineUnixMs = 0
		if !next.RegistrationAttempted {
			next.RegAbsent = true
		}
	})
}

// closeCreation is the trusted creator's one-shot completion attestation.
// created requires an independently observed owned server ID; rejected must
// not carry a server ID. Retries must match the original result exactly.
func (e *ciEnforcerEngine) closeCreation(ctx context.Context, leaseID, result, serverID string) (ciEnforcerStatus, error) {
	ctx, done, err := e.beginWork(ctx)
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	defer done()
	if result != "created" && result != "rejected" {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if result == "rejected" && serverID != "" {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if result == "created" && !ciLifecycleID.MatchString(serverID) {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	st, err := e.stateFor(leaseID)
	if err != nil {
		return ciEnforcerStatus{}, err
	}
	if st == nil {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	// Slow provider verification happens BEFORE the state lock; the guard
	// re-checks everything against the current committed clone under lock.
	l := st.snapshot()
	if l == nil || l.Settled {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	if !l.CreateSettled && result == "created" {
		owned, verr := e.verifyOwnedServer(ctx, l, serverID)
		if verr != nil {
			return ciEnforcerStatus{}, verr
		}
		if !owned {
			return ciEnforcerStatus{}, errCIEnforcerConflict
		}
	}
	return e.commitGuarded(st, func(next *ciEnforcerLease) error {
		if next.Settled {
			return errCIEnforcerConflict
		}
		if next.CreateSettled {
			match := next.CreateResult == result && (result != "created" || next.CreateServerID == serverID)
			if !match {
				return errCIEnforcerConflict
			}
			return errCIEnforcerRetry
		}
		return nil
	}, func(next *ciEnforcerLease) {
		if next.CreateSettled {
			return // exact retry: nothing to mutate
		}
		next.CreateSettled = true
		next.CreateResult = result
		next.CreateServerID = serverID
		if serverID != "" {
			next.Resources = appendUniqueResource(next.Resources, compute.CIRunnerResource{Kind: "server", ID: serverID, OperationID: next.Spec.Plan.OperationID})
			next.PhysicalAbsent = false
		}
	})
}

var errCIEnforcerRetry = errors.New("ci enforcer exact retry")

func appendUniqueResource(list []compute.CIRunnerResource, r compute.CIRunnerResource) []compute.CIRunnerResource {
	for _, x := range list {
		if x.Kind == r.Kind && x.ID == r.ID {
			return list
		}
	}
	return append(list, r)
}

// verifyOwnedServer independently confirms a created server ID is owned by
// this exact operation via a fresh provider GET or a previously retained ID.
func (e *ciEnforcerEngine) verifyOwnedServer(ctx context.Context, l *ciEnforcerLease, serverID string) (bool, error) {
	if e.compute == nil {
		return false, errCIEnforcerUnavailable
	}
	for _, r := range l.Resources {
		if r.Kind == "server" && r.ID == serverID {
			return true, nil
		}
	}
	server, err := e.compute.Server(ctx, serverID)
	if err != nil {
		if compute.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	scope := ciEnforcerScope(l.Spec)
	md := server.Metadata
	return server.ID == serverID && server.Name == scope.ServerName &&
		md["canter.managed"] == "true" && md["canter.sandbox"] == "ci-"+scope.OperationID &&
		md["canter.operation"] == scope.OperationID && md["canter.resource"] == scope.ServerName, nil
}

// registerRunner performs the service-owned one-shot JIT registration. A
// per-lease registering claim spans preflight and the sole POST; durable
// attempted=true is fsynced immediately before the POST after re-checking
// stop/expiry. State locks are never held across provider or GitHub I/O.
func (e *ciEnforcerEngine) registerRunner(ctx context.Context, leaseID string) (ciEnforcerStatus, int64, string, error) {
	ctx, done, err := e.beginWork(ctx)
	if err != nil {
		return ciEnforcerStatus{}, 0, "", err
	}
	defer done()
	st, err := e.stateFor(leaseID)
	if err != nil {
		return ciEnforcerStatus{}, 0, "", err
	}
	if st == nil {
		return ciEnforcerStatus{}, 0, "", errCIEnforcerConflict
	}
	if e.github == nil {
		return ciEnforcerStatus{}, 0, "", errCIEnforcerUnavailable
	}
	// Readiness and claiming are one transition, not a stale snapshot
	// followed by a claim after another registration has already completed.
	st.stateMu.Lock()
	l := cloneLease(st.lease)
	wall, mono := e.now()
	if st.unavailable || l == nil || l.Settled || l.StopRequested ||
		l.RegistrationSettled || l.RegistrationConflict || l.expired(wall, mono) ||
		!l.CreateSettled || l.CreateResult != "created" || l.CreateServerID == "" {
		st.stateMu.Unlock()
		return ciEnforcerStatus{}, 0, "", errCIEnforcerConflict
	}
	if st.registering || l.RegistrationAttempted {
		st.stateMu.Unlock()
		return ciEnforcerStatus{}, 0, "", errCIRunnerJobPending
	}
	st.registering = true
	st.stateMu.Unlock()
	defer func() {
		st.stateMu.Lock()
		st.registering = false
		st.stateMu.Unlock()
	}()

	// Preflight runs BEFORE the durable attempt is persisted: token, repo
	// resolution and complete runner listing. A transient preflight failure
	// leaves attempted=false so the call may retry; only the POST outcome is
	// ambiguous.
	pre, err := e.githubJITPreflight(ctx, l)
	if err != nil {
		if errors.Is(err, errCIEnforcerRunnerConflict) {
			_ = e.commit(st, func(next *ciEnforcerLease) {
				next.RegistrationConflict = true
			})
		}
		return ciEnforcerStatus{}, 0, "", err
	}
	// Re-check stop/expiry and persist the durable one-shot attempt under
	// the SAME lock: a stop landing between check and write can never be
	// followed by a POST.
	if _, err = e.commitGuarded(st, func(next *ciEnforcerLease) error {
		wall, mono = e.now()
		if next.Settled || next.StopRequested || next.expired(wall, mono) || next.RegistrationAttempted {
			return errCIEnforcerConflict
		}
		return nil
	}, func(next *ciEnforcerLease) {
		next.RegistrationAttempted = true
	}); err != nil {
		return ciEnforcerStatus{}, 0, "", err
	}

	runnerID, jit, definitive, err := e.githubJITPost(ctx, l, pre)

	st.stateMu.Lock()
	defer st.stateMu.Unlock()
	cur := st.lease
	if cur == nil || cur.Settled {
		return ciEnforcerStatus{}, 0, "", errCIEnforcerConflict
	}
	blocked := cur.StopRequested || cur.expired(e.now())
	rpcErr := err
	switch {
	case errors.Is(rpcErr, errCIEnforcerRunnerConflict):
		if perr := e.commitLocked(st, func(next *ciEnforcerLease) {
			next.RegistrationConflict = true
		}); perr != nil {
			return ciEnforcerStatus{}, 0, "", perr
		}
	case definitive && runnerID > 0:
		if perr := e.commitLocked(st, func(next *ciEnforcerLease) {
			next.GitHubRunnerID = runnerID
			next.RegistrationSettled = true
		}); perr != nil {
			return ciEnforcerStatus{}, 0, "", perr
		}
	case definitive && runnerID == 0:
		if perr := e.commitLocked(st, func(next *ciEnforcerLease) {
			next.RegistrationSettled = true
		}); perr != nil {
			return ciEnforcerStatus{}, 0, "", perr
		}
	default:
		// Ambiguous outcome stays unsettled for source recovery.
	}
	s := leaseStatus(st.lease)
	if rpcErr != nil {
		return s, 0, "", rpcErr
	}
	if blocked {
		// Stop/expiry during the RPC: the returned runner ID is retained for
		// cleanup but JIT configuration is never delivered.
		return s, 0, "", errCIEnforcerConflict
	}
	if jit == "" {
		return s, 0, "", errCIRunnerJobPending
	}
	return s, runnerID, jit, nil
}

// commitLocked persists under an already-held stateMu.
func (e *ciEnforcerEngine) commitLocked(st *ciEnforcerLeaseState, mutate func(*ciEnforcerLease)) error {
	if st.unavailable {
		return errCIEnforcerUnavailable
	}
	next := cloneLease(st.lease)
	mutate(next)
	if err := e.persistLease(next); err != nil {
		st.unavailable = true
		return err
	}
	st.lease = next
	return nil
}

// commitGuarded runs guard and mutate on a clone of the committed lease
// while stateMu is held, persists the replacement, publishes it, and returns
// the exact committed status. Guards therefore can never be bypassed by a
// concurrent transition: there is no snapshot-then-blind-commit window.
// The caller must NOT hold stateMu; this function acquires it.
func (e *ciEnforcerEngine) commitGuarded(st *ciEnforcerLeaseState, guard func(*ciEnforcerLease) error, mutate func(*ciEnforcerLease)) (ciEnforcerStatus, error) {
	st.stateMu.Lock()
	defer st.stateMu.Unlock()
	if st.unavailable {
		return ciEnforcerStatus{}, errCIEnforcerUnavailable
	}
	cur := st.lease
	if cur == nil {
		return ciEnforcerStatus{}, errCIEnforcerConflict
	}
	next := cloneLease(cur)
	if err := guard(next); err != nil {
		if errors.Is(err, errCIEnforcerRetry) {
			return leaseStatus(cur), nil
		}
		return leaseStatus(cur), err
	}
	mutate(next)
	if err := e.persistLease(next); err != nil {
		st.unavailable = true
		return ciEnforcerStatus{}, err
	}
	st.lease = next
	return leaseStatus(next), nil
}

// reconcilePhysical performs one bounded provider pass for a lease. It is
// serialized per lease via physicalMu and never touches GitHub.
func (e *ciEnforcerEngine) reconcilePhysical(ctx context.Context, leaseID string) {
	ctx, done, err := e.beginWork(ctx)
	if err != nil {
		return
	}
	defer done()
	st, err := e.stateFor(leaseID)
	if err != nil || st == nil {
		return
	}
	st.physicalMu.Lock()
	defer st.physicalMu.Unlock()
	e.reconcilePhysicalLocked(ctx, st, leaseID)
}

// reconcilePhysicalLocked performs the pass with physicalMu already held.
func (e *ciEnforcerEngine) reconcilePhysicalLocked(ctx context.Context, st *ciEnforcerLeaseState, leaseID string) {
	l := st.snapshot()
	if l == nil || l.Settled {
		return
	}
	wall, mono := e.now()
	expired := l.expired(wall, mono)
	if !l.StopRequested && !expired && !st.isUnavailable() {
		return
	}
	// Persist the irrevocable stop before any cleanup. A storage failure
	// latches the lease unavailable for approvals/JIT/settled acks but must
	// never strand already-known owned resources: proceed best-effort.
	if !l.StopRequested {
		_ = e.commit(st, func(next *ciEnforcerLease) {
			next.StopRequested = true
			next.ApprovedDeadlineUnixMs = 0
		})
		l = st.snapshot()
	}
	scope := ciEnforcerScope(l.Spec)
	retained := append([]compute.CIRunnerResource(nil), l.Resources...)
	obs, obsErr := e.compute.ObserveCIRunner(ctx, scope, retained)
	if obsErr != nil {
		log.Printf("ci enforcer physical %s: observe: %v", leaseID, obsErr)
		_ = e.commit(st, func(next *ciEnforcerLease) { next.PhysicalAbsent = false })
		// Best-effort destroy of already-retained IDs only; compute methods
		// revalidate exact ownership before mutating. Never guess new IDs.
		if len(retained) > 0 {
			_ = e.compute.StopCIRunner(ctx, scope, retained)
			_ = e.compute.DeleteCIRunner(ctx, scope, retained)
		}
		return
	}
	combined := mergeResources(retained, obs.Discovered)
	// A storage failure latches the lease unavailable for approvals/JIT/
	// settled acks but must never strand already-known owned resources:
	// proceed with the in-memory union; compute revalidates every target.
	_ = e.commit(st, func(next *ciEnforcerLease) {
		next.Resources = mergeResources(next.Resources, combined)
		if len(obs.Discovered) != 0 {
			next.PhysicalAbsent = false
		}
		for _, read := range obs.Reads {
			if read.HTTPStatus != 404 {
				next.PhysicalAbsent = false
			}
		}
	})
	_ = e.compute.StopCIRunner(ctx, scope, combined)
	if err := e.compute.DeleteCIRunner(ctx, scope, combined); err != nil {
		log.Printf("ci enforcer physical %s: delete: %v", leaseID, err)
		return
	}
	// Fresh exact absence proof.
	obs2, err := e.compute.ObserveCIRunner(ctx, scope, combined)
	if err != nil {
		log.Printf("ci enforcer physical %s: reobserve: %v", leaseID, err)
		return
	}
	if !ciEnforcerAbsenceProved(l.Spec.Plan.OperationID, combined, obs2) {
		log.Printf("ci enforcer physical %s: absence not proved", leaseID)
		return
	}
	if _, err := e.commitGuarded(st, func(next *ciEnforcerLease) error {
		// A concurrent creator may have retained another server after the
		// provider read. That ID needs its own fresh absence proof.
		if next.Settled || !ciEnforcerAbsenceProved(next.Spec.Plan.OperationID, next.Resources, obs2) {
			return errCIRunnerJobPending
		}
		return nil
	}, func(next *ciEnforcerLease) {
		next.PhysicalAbsent = true
		next.Resources = mergeResources(next.Resources, combined)
	}); err != nil {
		log.Printf("ci enforcer physical %s: absence commit: %v", leaseID, err)
		return
	}
	e.maybeTombstone(st)
}

// maybeTombstone finalizes the lease only when every independent proof is
// durable: creation closed, physical absence freshly proven, GitHub
// registration resolved and absent, no in-flight registration, and storage
// healthy. The tombstone erases private metadata and resource IDs.
func (e *ciEnforcerEngine) maybeTombstone(st *ciEnforcerLeaseState) {
	st.stateMu.Lock()
	defer st.stateMu.Unlock()
	l := st.lease
	if l == nil || l.Settled || !l.StopRequested || l.RegistrationConflict || st.unavailable || st.registering {
		return
	}
	if !l.CreateSettled || !l.PhysicalAbsent || !l.RegAbsent {
		return
	}
	if l.RegistrationAttempted && !l.RegistrationSettled {
		return
	}
	if l.RegistrationSettled && l.GitHubRunnerID > 0 {
		return
	}
	tombstone := &ciEnforcerLease{
		LeaseID: l.LeaseID, SpecDigest: l.SpecDigest, PlanDigest: l.PlanDigest,
		StopRequested: true, Settled: true, PhysicalAbsent: true, RegAbsent: true,
	}
	tombstone.Spec.Plan.OperationID = l.Spec.Plan.OperationID
	tombstone.Spec.Plan.EnforcerLease = l.LeaseID
	if err := e.persistLease(tombstone); err != nil {
		st.unavailable = true
		return
	}
	st.lease = tombstone
}

// reconcileGitHub performs GitHub-side registration cleanup and recovery.
// It runs only after physical absence is proven and never blocks provider
// passes.
func (e *ciEnforcerEngine) reconcileGitHub(ctx context.Context, leaseID string) {
	ctx, done, err := e.beginWork(ctx)
	if err != nil {
		return
	}
	defer done()
	st, err := e.stateFor(leaseID)
	if err != nil || st == nil {
		return
	}
	l := st.snapshot()
	if l == nil || l.Settled || !l.StopRequested || !l.PhysicalAbsent {
		return
	}
	st.stateMu.Lock()
	if st.registering {
		st.stateMu.Unlock()
		return
	}
	st.registering = true
	st.stateMu.Unlock()
	defer func() {
		st.stateMu.Lock()
		st.registering = false
		st.stateMu.Unlock()
	}()

	attempted := l.RegistrationAttempted
	settled := l.RegistrationSettled
	conflicted := l.RegistrationConflict
	runnerID := l.GitHubRunnerID
	if !attempted {
		// Durable stop with no attempt proves no Canter registration exists.
		_ = e.commit(st, func(next *ciEnforcerLease) { next.RegAbsent = true })
		return
	}
	if e.github == nil {
		return
	}
	if conflicted {
		return
	}
	if !settled {
		// Unknown POST outcome: recover via complete source discovery.
		id, rerr := e.githubRecoverRunnerID(ctx, l.Spec)
		if rerr != nil {
			log.Printf("ci enforcer github %s: recover runner id: %v", leaseID, rerr)
			return
		}
		if id <= 0 {
			log.Printf("ci enforcer github %s: no runner discovered", leaseID)
			return
		}
		if err := e.commit(st, func(next *ciEnforcerLease) {
			next.GitHubRunnerID = id
			next.RegistrationSettled = true
		}); err != nil {
			return
		}
		l = st.snapshot()
		runnerID = l.GitHubRunnerID
	}
	if runnerID <= 0 {
		_ = e.commit(st, func(next *ciEnforcerLease) { next.RegAbsent = true })
		return
	}
	// Positive identity: exact-ID cleanup with independent absence proof.
	gone, err := e.githubDeleteRunner(ctx, l.Spec, runnerID)
	if err != nil || !gone {
		log.Printf("ci enforcer github %s: delete runner %d: gone=%v err=%v", leaseID, runnerID, gone, err)
		return
	}
	_ = e.commit(st, func(next *ciEnforcerLease) {
		next.GitHubRunnerID = 0
		next.RegAbsent = true
	})
}

// recoverLiveRegistration resolves an ambiguous JIT POST for a still-live
// lease via complete discovery; used while the lease has not expired.
func (e *ciEnforcerEngine) recoverLiveRegistration(ctx context.Context, leaseID string) {
	ctx, done, err := e.beginWork(ctx)
	if err != nil {
		return
	}
	defer done()
	st, err := e.stateFor(leaseID)
	if err != nil || st == nil {
		return
	}
	l := st.snapshot()
	if l == nil || l.Settled || l.StopRequested || !l.RegistrationAttempted ||
		l.RegistrationSettled || l.RegistrationConflict {
		return
	}
	if e.github == nil {
		return
	}
	st.stateMu.Lock()
	if st.registering {
		st.stateMu.Unlock()
		return
	}
	st.registering = true
	st.stateMu.Unlock()
	defer func() {
		st.stateMu.Lock()
		st.registering = false
		st.stateMu.Unlock()
	}()
	id, err := e.githubRecoverRunnerID(ctx, l.Spec)
	if err != nil || id <= 0 {
		return
	}
	_ = e.commit(st, func(next *ciEnforcerLease) {
		next.GitHubRunnerID = id
		next.RegistrationSettled = true
	})
}

func mergeResources(retained, discovered []compute.CIRunnerResource) []compute.CIRunnerResource {
	out := append([]compute.CIRunnerResource(nil), retained...)
	seen := make(map[string]bool, len(out))
	for _, r := range out {
		seen[r.Kind+"/"+r.ID] = true
	}
	for _, r := range discovered {
		k := r.Kind + "/" + r.ID
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ciEnforcerAbsenceProved proves PHYSICAL absence only, through the root
// evidence function: every expected retained ID must have a direct 404 read,
// discovery must be complete and empty, and every known server must have a
// complete empty port list. Creator-closure and GitHub proofs are gated
// independently by the lease state machine; this flag says nothing about them.
func ciEnforcerAbsenceProved(operation string, expected []compute.CIRunnerResource, obs compute.CIRunnerObservation) bool {
	retained := make([]ciOwnedResource, 0, len(expected))
	for _, r := range expected {
		retained = append(retained, ciOwnedResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
	}
	conv := ciResourceObservation{
		OperationID:       operation,
		Retained:          retained,
		Discovered:        make([]ciOwnedResource, 0, len(obs.Discovered)),
		Reads:             make([]ciResourceRead, 0, len(obs.Reads)),
		ServerPorts:       make([]ciServerPorts, 0, len(obs.ServerPorts)),
		DiscoveryComplete: obs.DiscoveryComplete,
		// Physical-only gate: both flags forced true so ciResourceEvidence
		// evaluates absence; the caller still requires CreateSettled and
		// registration proofs before any tombstone.
		CreateSettled:   true,
		EnforcerSettled: true,
	}
	for _, r := range obs.Discovered {
		conv.Discovered = append(conv.Discovered, ciOwnedResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
	}
	for _, r := range obs.Reads {
		conv.Reads = append(conv.Reads, ciResourceRead{
			Resource:    ciOwnedResource{Kind: r.Resource.Kind, ID: r.Resource.ID, OperationID: r.Resource.OperationID},
			HTTPStatus:  r.HTTPStatus,
			ServerState: r.ServerState,
		})
	}
	for _, p := range obs.ServerPorts {
		conv.ServerPorts = append(conv.ServerPorts, ciServerPorts{ServerID: p.ServerID, Complete: p.Complete, PortIDs: p.PortIDs})
	}
	_, _, absent, err := ciResourceEvidence(operation, retained, conv)
	return err == nil && absent
}

// ciEnforcerRoundRobin orders lease IDs deterministically for fair dispatch:
// the sorted copy rotates so the first ID greater than `after` leads. The
// input is never mutated; an empty cursor or a cursor past the last ID
// starts at the sorted head.
func ciEnforcerRoundRobin(ids []string, after string) []string {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	split := sort.Search(len(sorted), func(i int) bool { return sorted[i] > after })
	if split == len(sorted) {
		split = 0
	}
	out := make([]string, 0, len(sorted))
	out = append(out, sorted[split:]...)
	return append(out, sorted[:split]...)
}

// Run reconciles leases once per second: physical provider passes and
// GitHub passes are dispatched independently with bounded concurrency and
// per-lease serialization, so a blocked GitHub call never occupies a
// physical slot.
func (e *ciEnforcerEngine) Run(ctx context.Context) {
	e.mu.Lock()
	if e.closed || e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.runWg.Add(1)
	e.mu.Unlock()
	rctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer func() {
		stop()
		cancel()
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
		e.runWg.Done()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	physSem := make(chan struct{}, 2)
	ghSem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	defer wg.Wait()
	// Per-Run dispatch cursors: each phase rotates independently so a full
	// pool or a blocked pass in one phase can never starve the other, and
	// no lease can be skipped forever by map iteration order.
	var lastPhysical, lastGitHub string
	for {
		select {
		case <-rctx.Done():
			return
		case <-ticker.C:
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return
		}
		pending := make(map[string]*ciEnforcerLeaseState, len(e.leases))
		ids := make([]string, 0, len(e.leases))
		for _, st := range e.leases {
			l := st.snapshot()
			if l != nil && !l.Settled {
				pending[l.LeaseID] = st
				ids = append(ids, l.LeaseID)
			}
		}
		e.mu.Unlock()
		wall, mono := e.now()
		// Physical phase: the rotation order is fixed for this tick; the
		// cursor advances only on a successful dispatch.
		for _, id := range ciEnforcerRoundRobin(ids, lastPhysical) {
			st := pending[id]
			l := st.snapshot()
			if l == nil || l.Settled {
				continue
			}
			expired := l.expired(wall, mono)
			unavailable := st.isUnavailable()
			if !(l.StopRequested || expired || unavailable) {
				continue
			}
			// Nonblocking dispatch: a full physical pool or an
			// already-running pass for this lease skips this tick
			// rather than stalling the timer or holding a slot.
			if !st.physicalMu.TryLock() {
				continue
			}
			select {
			case physSem <- struct{}{}:
			default:
				st.physicalMu.Unlock()
				continue
			}
			lastPhysical = id
			wg.Add(1)
			go func(st *ciEnforcerLeaseState, id string) {
				defer wg.Done()
				defer func() { <-physSem }()
				defer st.physicalMu.Unlock()
				wr, cancel := context.WithTimeout(rctx, 90*time.Second)
				defer cancel()
				e.reconcilePhysicalLocked(wr, st, id)
			}(st, id)
		}
		// GitHub phase: independent cursor and fresh snapshots, which may
		// observe physical proofs committed earlier in this same tick. A
		// skipped physical dispatch can never skip this phase.
		for _, id := range ciEnforcerRoundRobin(ids, lastGitHub) {
			st := pending[id]
			l := st.snapshot()
			if l == nil || l.Settled {
				continue
			}
			expired := l.expired(wall, mono)
			unavailable := st.isUnavailable()
			needsGH := !unavailable && (l.StopRequested && l.PhysicalAbsent ||
				!l.StopRequested && !expired && l.RegistrationAttempted && !l.RegistrationSettled && !l.RegistrationConflict)
			if !needsGH {
				continue
			}
			select {
			case ghSem <- struct{}{}:
			default:
				continue
			}
			lastGitHub = id
			wg.Add(1)
			go func(id string, stopped bool) {
				defer wg.Done()
				defer func() { <-ghSem }()
				wr, cancel := context.WithTimeout(rctx, 90*time.Second)
				defer cancel()
				if stopped {
					e.reconcileGitHub(wr, id)
				} else {
					e.recoverLiveRegistration(wr, id)
				}
			}(id, l.StopRequested && l.PhysicalAbsent)
		}
	}
}

func (s *ciEnforcerLeaseState) leaseID() string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.lease == nil {
		return ""
	}
	return s.lease.LeaseID
}

// close forbids new work first, then cancels and joins the Run loop and all
// in-flight provider/GitHub I/O, and only then releases the process flock.
func (e *ciEnforcerEngine) close() {
	e.mu.Lock()
	e.closed = true
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.runWg.Wait()
	e.inflight.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flock != nil {
		e.flock.Close()
		e.flock = nil
	}
}

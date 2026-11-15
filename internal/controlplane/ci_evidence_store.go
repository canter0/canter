package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var (
	ciEvidenceBatchKeys        = map[string]bool{"schemaVersion": true, "batchId": true, "metrics": true, "tests": true}
	ciEvidenceMetricKeys       = map[string]bool{"at": true, "cpuPercent": true, "memoryBytes": true, "memoryLimitBytes": true, "networkRxBytes": true, "networkTxBytes": true, "oomKills": true}
	ciEvidenceTestKeys         = map[string]bool{"id": true, "name": true, "suite": true, "status": true, "message": true, "durationMs": true}
	ciEvidenceRequiredTestKeys = map[string]bool{"id": true, "name": true, "suite": true, "status": true, "message": true}
)

// ciEvidenceJSONObject decodes one JSON object under an exact allowed key
// set. Duplicate keys, unknown keys (case aliases included), missing
// required keys, non-object roots, and trailing data are all rejected.
func ciEvidenceJSONObject(raw []byte, allowed, required map[string]bool) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, errCIEvidenceInvalid
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errCIEvidenceInvalid
	}
	values := make(map[string]json.RawMessage, len(allowed))
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, errCIEvidenceInvalid
		}
		key, ok := keyTok.(string)
		if !ok || !allowed[key] {
			return nil, errCIEvidenceInvalid
		}
		if _, dup := values[key]; dup {
			return nil, errCIEvidenceInvalid
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errCIEvidenceInvalid
		}
		values[key] = value
	}
	if tok, err = dec.Token(); err != nil {
		return nil, errCIEvidenceInvalid
	} else if d, ok := tok.(json.Delim); !ok || d != '}' {
		return nil, errCIEvidenceInvalid
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errCIEvidenceInvalid
	}
	for key := range required {
		if _, ok := values[key]; !ok {
			return nil, errCIEvidenceInvalid
		}
	}
	return values, nil
}

func ciEvidenceJSONString(fields map[string]json.RawMessage, key string, dst *string) error {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errCIEvidenceInvalid
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errCIEvidenceInvalid
	}
	return nil
}

func ciEvidenceJSONInt(fields map[string]json.RawMessage, key string, dst *int64) error {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errCIEvidenceInvalid
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errCIEvidenceInvalid
	}
	return nil
}

// ciEvidenceMetricNumber decodes one optional numeric observation: absent or
// explicit null stays nil (unknown); a present value must be a JSON number of
// the destination type. Source is never an accepted key.
func ciEvidenceMetricNumber(fields map[string]json.RawMessage, key string, dst any) error {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(trimmed, dst); err != nil {
		return errCIEvidenceInvalid
	}
	return nil
}

func ciEvidenceArrayElements(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errCIEvidenceInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return nil, errCIEvidenceInvalid
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errCIEvidenceInvalid
	}
	var elements []json.RawMessage
	for dec.More() {
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil, errCIEvidenceInvalid
		}
		elements = append(elements, element)
	}
	if tok, err = dec.Token(); err != nil {
		return nil, errCIEvidenceInvalid
	} else if d, ok := tok.(json.Delim); !ok || d != ']' {
		return nil, errCIEvidenceInvalid
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errCIEvidenceInvalid
	}
	return elements, nil
}

func decodeCIEvidenceMetric(raw json.RawMessage) (CIMetric, error) {
	var m CIMetric
	fields, err := ciEvidenceJSONObject(raw, ciEvidenceMetricKeys, map[string]bool{"at": true})
	if err != nil {
		return m, err
	}
	atRaw := fields["at"]
	if bytes.Equal(bytes.TrimSpace(atRaw), []byte("null")) {
		return m, errCIEvidenceInvalid
	}
	if err := json.Unmarshal(atRaw, &m.At); err != nil {
		return m, errCIEvidenceInvalid
	}
	if err := ciEvidenceMetricNumber(fields, "cpuPercent", &m.CPUPercent); err != nil {
		return m, err
	}
	if err := ciEvidenceMetricNumber(fields, "memoryBytes", &m.MemoryBytes); err != nil {
		return m, err
	}
	if err := ciEvidenceMetricNumber(fields, "memoryLimitBytes", &m.MemoryLimitBytes); err != nil {
		return m, err
	}
	if err := ciEvidenceMetricNumber(fields, "networkRxBytes", &m.NetworkRxBytes); err != nil {
		return m, err
	}
	if err := ciEvidenceMetricNumber(fields, "networkTxBytes", &m.NetworkTxBytes); err != nil {
		return m, err
	}
	if err := ciEvidenceMetricNumber(fields, "oomKills", &m.OOMKills); err != nil {
		return m, err
	}
	return m, nil
}

func decodeCIEvidenceTest(raw json.RawMessage) (CITestResult, error) {
	var t CITestResult
	fields, err := ciEvidenceJSONObject(raw, ciEvidenceTestKeys, ciEvidenceRequiredTestKeys)
	if err != nil {
		return t, err
	}
	if err := ciEvidenceJSONString(fields, "id", &t.ID); err != nil {
		return t, err
	}
	if err := ciEvidenceJSONString(fields, "name", &t.Name); err != nil {
		return t, err
	}
	if err := ciEvidenceJSONString(fields, "suite", &t.Suite); err != nil {
		return t, err
	}
	if err := ciEvidenceJSONString(fields, "status", &t.Status); err != nil {
		return t, err
	}
	if err := ciEvidenceJSONString(fields, "message", &t.Message); err != nil {
		return t, err
	}
	if raw, ok := fields["durationMs"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if !bytes.Equal(trimmed, []byte("null")) {
			if err := json.Unmarshal(trimmed, &t.DurationMs); err != nil {
				return t, errCIEvidenceInvalid
			}
		}
	}
	return t, nil
}

// decodeCIEvidenceBatch parses the raw request body itself so a typed batch
// can never be confused with the hashed bytes. The envelope requires exactly
// schemaVersion=1, a 64-hex batchId, and non-null metrics/tests arrays;
// source is never an accepted key at any level.
func decodeCIEvidenceBatch(raw []byte) (ciEvidenceBatch, error) {
	var batch ciEvidenceBatch
	if len(raw) == 0 || len(raw) > ciEvidenceRequestMaxBytes || !utf8.Valid(raw) {
		return batch, errCIEvidenceInvalid
	}
	fields, err := ciEvidenceJSONObject(raw, ciEvidenceBatchKeys, ciEvidenceBatchKeys)
	if err != nil {
		return batch, err
	}
	var schema int64
	if err := ciEvidenceJSONInt(fields, "schemaVersion", &schema); err != nil || schema != 1 {
		return batch, errCIEvidenceInvalid
	}
	batch.SchemaVersion = 1
	if err := ciEvidenceJSONString(fields, "batchId", &batch.BatchID); err != nil ||
		!ciLifecycleLeaseID.MatchString(batch.BatchID) {
		return batch, errCIEvidenceInvalid
	}
	metricElems, err := ciEvidenceArrayElements(fields["metrics"])
	if err != nil {
		return batch, err
	}
	testElems, err := ciEvidenceArrayElements(fields["tests"])
	if err != nil {
		return batch, err
	}
	batch.Metrics = make([]CIMetric, 0, len(metricElems))
	for _, element := range metricElems {
		metric, err := decodeCIEvidenceMetric(element)
		if err != nil {
			return batch, err
		}
		batch.Metrics = append(batch.Metrics, metric)
	}
	batch.Tests = make([]CITestResult, 0, len(testElems))
	for _, element := range testElems {
		test, err := decodeCIEvidenceTest(element)
		if err != nil {
			return batch, err
		}
		batch.Tests = append(batch.Tests, test)
	}
	return batch, nil
}

// ciEvidenceScopeQuery loads the twenty-field authoritative scope under the
// workspace lock. Unknown or ineligible runners are unauthorized, never a
// distinct oracle.
func ciEvidenceScopeQuery(ctx context.Context, tx pgx.Tx, workspace, runner string) (ciEvidenceScope, error) {
	var scope ciEvidenceScope
	err := tx.QueryRow(ctx, ciEvidenceScopeSQL, workspace, runner).Scan(
		&scope.Workspace, &scope.Job, &scope.Runner, &scope.SizeID,
		&scope.GitHubInstallationID, &scope.GitHubRepositoryID,
		&scope.GitHubJobID, &scope.GitHubRunID,
		&scope.AdmissionID, &scope.RunnerName, &scope.AdmissionRepositoryID,
		&scope.CreatedAt, &scope.StartedAt, &scope.DeadlineAt, &scope.HardDeadlineAt,
		&scope.StoppedAt, &scope.StopRequestedAt, &scope.BillStopAt,
		&scope.Decision, &scope.GitHubRunnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciEvidenceScope{}, errCIEvidenceUnauthorized
	}
	if err != nil {
		return ciEvidenceScope{}, err
	}
	return scope, nil
}

func ciEvidenceWorkspaceLock(ctx context.Context, tx pgx.Tx, workspace string) error {
	var locked string
	err := tx.QueryRow(ctx, ciEvidenceWorkspaceLockSQL, workspace).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return errCIEvidenceUnauthorized
	}
	return err
}

// loadCIEvidenceScope re-reads the authoritative scope and verifies the
// capability window inside one read-only transaction before any body work.
func (s *Store) loadCIEvidenceScope(ctx context.Context, c ciEvidenceCapability) (ciEvidenceScope, error) {
	if !c.verified {
		return ciEvidenceScope{}, errCIEvidenceUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciEvidenceScope{}, err
	}
	defer tx.Rollback(ctx)
	if err := ciEvidenceWorkspaceLock(ctx, tx, c.Workspace); err != nil {
		return ciEvidenceScope{}, err
	}
	scope, err := ciEvidenceScopeQuery(ctx, tx, c.Workspace, c.Runner)
	if err != nil {
		return ciEvidenceScope{}, err
	}
	if _, err := scope.window(c, s.now()); err != nil {
		return ciEvidenceScope{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ciEvidenceScope{}, err
	}
	return scope, nil
}

// acceptCIEvidence stores one authenticated batch atomically: workspace lock,
// authoritative scope, capability window, normalization, batch idempotence,
// per-observation dedupe, quota accounting, a final window recheck, then the
// batch marker — all in one transaction. Evidence never creates jobs,
// runners, lifecycle rows, or funding changes.
func (s *Store) acceptCIEvidence(ctx context.Context, c ciEvidenceCapability, rawBody []byte) error {
	if !c.verified {
		return errCIEvidenceUnauthorized
	}
	if len(rawBody) == 0 || len(rawBody) > ciEvidenceRequestMaxBytes {
		return errCIEvidenceInvalid
	}
	batch, err := decodeCIEvidenceBatch(rawBody)
	if err != nil {
		return err
	}
	rawSum := sha256.Sum256(rawBody)
	rawSHA := hex.EncodeToString(rawSum[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := ciEvidenceWorkspaceLock(ctx, tx, c.Workspace); err != nil {
		return err
	}
	scope, err := ciEvidenceScopeQuery(ctx, tx, c.Workspace, c.Runner)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	cutoff, err := scope.window(c, now)
	if err != nil {
		return err
	}
	normalized, err := normalizeCIEvidence(batch, scope, cutoff, now)
	if err != nil {
		return err
	}
	var existingSHA string
	err = tx.QueryRow(ctx, ciEvidenceBatchSQL, scope.Workspace, scope.Job, batch.BatchID).Scan(&existingSHA)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case existingSHA == rawSHA:
		// Exact retry of an already-accepted batch: commit without touching
		// counters, even when the quota is now full.
		return tx.Commit(ctx)
	default:
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, ciEvidenceUsageInitSQL, scope.Workspace, scope.Job); err != nil {
		return err
	}
	var metricCount, testCount, batchCount int
	var acceptedBytes int64
	if err = tx.QueryRow(ctx, ciEvidenceUsageSQL, scope.Workspace, scope.Job).
		Scan(&metricCount, &testCount, &batchCount, &acceptedBytes); err != nil {
		return err
	}
	newMetrics, newTests := 0, 0
	for _, metric := range normalized.Metrics {
		doc, err := json.Marshal(metric)
		if err != nil {
			return err
		}
		var same bool
		err = tx.QueryRow(ctx, ciEvidenceMetricExistsSQL,
			scope.Workspace, scope.Job, metric.At, string(doc)).Scan(&same)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err = tx.Exec(ctx, ciEvidenceMetricInsertSQL,
				scope.Workspace, scope.Job, metric.At, string(doc)); err != nil {
				return err
			}
			newMetrics++
			continue
		}
		if err != nil {
			return err
		}
		if !same {
			return ErrConflict
		}
	}
	for _, test := range normalized.Tests {
		doc, err := json.Marshal(test)
		if err != nil {
			return err
		}
		var same bool
		err = tx.QueryRow(ctx, ciEvidenceTestExistsSQL,
			scope.Workspace, scope.Job, test.ID, string(doc)).Scan(&same)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err = tx.Exec(ctx, ciEvidenceTestInsertSQL,
				scope.Workspace, scope.Job, test.ID, string(doc)); err != nil {
				return err
			}
			newTests++
			continue
		}
		if err != nil {
			return err
		}
		if !same {
			return ErrConflict
		}
	}
	tag, err := tx.Exec(ctx, ciEvidenceUsageUpdateSQL,
		scope.Workspace, scope.Job, newMetrics, newTests, len(rawBody))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errCIEvidenceQuota
	}
	// The upload grace may not be crossed by slow writes: re-verify the
	// window with a fresh clock before the batch marker commits.
	freshNow := s.now().UTC().Truncate(time.Microsecond)
	if _, err := scope.window(c, freshNow); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, ciEvidenceBatchInsertSQL,
		scope.Workspace, scope.Job, batch.BatchID, rawSHA, freshNow); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

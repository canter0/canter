package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/canter0/canter/pricing"
)

func (h *HTTPServer) workspaceCI(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string, parts []string) {
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		h.ciOverview(w, r, p, workspaceID)
		return
	}
	if len(parts) == 1 && parts[0] == "settings" {
		if r.Method != http.MethodPatch {
			methodNotAllowed(w)
			return
		}
		h.ciSettingsPatch(w, r, p, workspaceID)
		return
	}
	if len(parts) == 2 && parts[0] == "jobs" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		h.serveCIJobDetail(w, r, p, workspaceID, parts[1])
		return
	}
	if len(parts) == 3 && parts[0] == "jobs" && parts[2] == "logs" && h.service.Store.canReadCILogRanges(workspaceID) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		h.ciJobLogPage(w, r, p, workspaceID, parts[1])
		return
	}
	if len(parts) == 3 && parts[0] == "jobs" && parts[2] == "cancel" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		h.ciJobCancel(w, r, p, workspaceID, parts[1])
		return
	}
	if len(parts) == 2 && parts[0] == "github" && (parts[1] == "connect" || parts[1] == "link-personal") {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		h.ciConnect(w, r, p, workspaceID, parts[1] == "link-personal")
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) ciOverview(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string) {
	view, err := h.ciOverviewView(r.Context(), p, workspaceID)
	if errors.Is(err, errCIRepoUnavailable) {
		writeError(w, http.StatusServiceUnavailable, errors.New("Repository access could not be verified."))
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTPServer) ciOverviewView(ctx context.Context, p Principal, workspaceID string) (CIOverview, error) {
	enabled, err := h.service.Store.CIProductEnabled(ctx, workspaceID)
	if err != nil {
		return CIOverview{}, err
	}
	settings, err := h.service.Store.CISettings(ctx, workspaceID)
	if err != nil {
		return CIOverview{}, err
	}
	installations, err := h.service.Store.ListCIInstallations(ctx, workspaceID)
	if err != nil {
		return CIOverview{}, err
	}
	rows, err := h.service.Store.listCIJobRows(ctx, workspaceID, 200)
	if err != nil {
		return CIOverview{}, err
	}
	jobs, err := h.ciVisibleJobs(ctx, p, workspaceID, rows)
	if err != nil {
		return CIOverview{}, err
	}
	cacheReady := h.config.CI.Cache != nil && h.config.CI.Cache.available(ctx)
	pool := ciPoolOffline
	if h.config.CI.ManagedService != nil {
		pool = h.config.CI.ManagedService.pool(ctx)
	} else if h.config.CI.Managed != nil {
		pool = h.config.CI.Managed.Pool(ctx)
	}
	features := ciFeatureList(h.ciConfigured(), cacheReady)
	logsAvailable := h.config.CI.Managed != nil && h.config.CI.Managed.logsAvailable()
	if s := h.config.CI.ManagedService; s != nil {
		s.mu.Lock()
		logsAvailable = s.state == "running" && s.handle != nil && s.handle.config.Managed.logsAvailable()
		s.mu.Unlock()
	}
	for i := range features {
		if features[i].ID == "runner-pool" {
			if pool.Ready {
				features[i].Status = "available"
			} else {
				features[i].Status = pool.Status
			}
			if pool.Message != "" {
				features[i].Description = pool.Message
			}
		}
		if features[i].ID == "job-logs" && logsAvailable {
			features[i].Status = "available"
			features[i].Description = "Completed managed-job logs are collected from GitHub. Live console streaming is not enabled."
		}
		if features[i].ID == "job-metrics" && h.ciEvidenceAvailable() {
			features[i].Status = "available"
			features[i].Description = "Guest-reported VM metrics are collected after admission. Submit JUnit files with canter-ci-report junit FILE."
		}
	}
	catalog, _ := pricing.CICatalogForVersion(h.service.Store.ciAdmissionRateVersion(workspaceID))
	return CIOverview{
		Enabled:       enabled,
		Pool:          pool,
		Pricing:       catalog,
		Settings:      settings,
		Installations: installations,
		Jobs:          jobs,
		Features:      features,
	}, nil
}

var errCIRepoUnavailable = errors.New("repository access could not be verified")

func (h *HTTPServer) ciRepoAccess(ctx context.Context, p Principal, workspaceID string, cache map[string]bool, repoID int64, fullName string) (bool, error) {
	if repoID <= 0 {
		return false, nil
	}
	key := fmt.Sprintf("%d/%s", repoID, fullName)
	if v, ok := cache[key]; ok {
		return v, nil
	}
	visible, err := h.ciCheckRepoAccess(ctx, p, workspaceID, repoID, fullName)
	if err != nil {
		return false, err
	}
	cache[key] = visible
	return visible, nil
}

func (h *HTTPServer) ciCheckRepoAccess(ctx context.Context, p Principal, workspaceID string, repoID int64, fullName string) (bool, error) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false, nil
	}
	userToken := ""
	if p.Account != nil && p.Installation == nil && p.Session == nil {
		_, token, err := h.githubAccess(ctx, p.Account.ID, workspaceID)
		if err != nil {
			return false, errCIRepoUnavailable
		}
		userToken = token
	}
	client := h.ciGitHubTransport()
	info, err := client.repoInfo(ctx, fullName, userToken)
	if err != nil {
		var ghErr *ciGitHubError
		if errors.As(err, &ghErr) {
			if ghErr.status == http.StatusNotFound || ghErr.status == http.StatusForbidden || ghErr.status == http.StatusUnauthorized {
				return false, nil
			}
			return false, errCIRepoUnavailable
		}
		return false, errCIRepoUnavailable
	}
	if info.ID != repoID {
		return false, nil
	}
	if userToken != "" {
		return true, nil
	}
	return info.Private != nil && !*info.Private, nil
}

func (h *HTTPServer) ciVisibleJobs(ctx context.Context, p Principal, workspaceID string, rows []ciJobRow) ([]CIJob, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cache := map[string]bool{}
	jobs := []CIJob{}
	for _, row := range rows {
		ok, err := h.ciRepoAccess(ctx, p, workspaceID, cache, row.GitHubRepositoryID, row.Repository)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		jobs = append(jobs, row.Job)
	}
	return jobs, nil
}

func (h *HTTPServer) ciJobVisible(ctx context.Context, p Principal, workspaceID string, row ciJobRow) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return h.ciRepoAccess(ctx, p, workspaceID, map[string]bool{}, row.GitHubRepositoryID, row.Repository)
}

func (h *HTTPServer) serveCIJobDetail(w http.ResponseWriter, r *http.Request, p Principal, workspaceID, jobID string) {
	view, err := h.ciJobDetail(r.Context(), p, workspaceID, jobID, h.service.Store.canReadCILogRanges(workspaceID) && r.URL.Query().Get("logs") == "range")
	if errors.Is(err, errCIRepoUnavailable) {
		writeError(w, http.StatusServiceUnavailable, errors.New("Repository access could not be verified."))
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *HTTPServer) ciJobDetailView(ctx context.Context, p Principal, workspaceID, jobID string) (CIJobDetail, error) {
	return h.ciJobDetail(ctx, p, workspaceID, jobID, false)
}

func (h *HTTPServer) ciJobDetail(ctx context.Context, p Principal, workspaceID, jobID string, rangeLogs bool) (CIJobDetail, error) {
	row, steps, err := h.service.Store.CIJobRow(ctx, workspaceID, jobID)
	if err != nil {
		return CIJobDetail{}, err
	}
	visible, err := h.ciJobVisible(ctx, p, workspaceID, row)
	if err != nil {
		return CIJobDetail{}, err
	}
	if !visible {
		return CIJobDetail{}, ErrNotFound
	}
	logs := []CILogEntry{}
	if !rangeLogs {
		logs, err = h.service.Store.CIJobLogs(ctx, workspaceID, jobID)
		if err != nil {
			return CIJobDetail{}, err
		}
	}
	metrics, err := h.service.Store.CIJobMetrics(ctx, workspaceID, jobID)
	if err != nil {
		return CIJobDetail{}, err
	}
	tests, err := h.service.Store.CIJobTests(ctx, workspaceID, jobID)
	if err != nil {
		return CIJobDetail{}, err
	}
	diagnostics, err := h.service.Store.ciJobRunnerDiagnostics(ctx, workspaceID, jobID)
	if err != nil {
		return CIJobDetail{}, err
	}
	collection, err := h.service.Store.CIJobLogCollection(ctx, workspaceID, jobID)
	if err != nil {
		return CIJobDetail{}, err
	}
	view := CIJobDetail{Job: row.Job, Steps: steps, Logs: logs, Metrics: metrics, Tests: tests, RunnerDiagnostics: diagnostics, LogCollection: collection, RangeLogs: rangeLogs}
	if diagnostics != nil && diagnostics.Association == "assigned" {
		view.PinnedRate = &diagnostics.pinnedRate
	}
	return view, nil
}

func (h *HTTPServer) ciSettingsPatch(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string) {
	if p.Account == nil || p.Installation != nil || p.Session != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	var settings CISettings
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid CI settings body"))
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("invalid CI settings body"))
		return
	}
	if settings.MonthlySpendLimitCents < 1 || settings.MonthlySpendLimitCents > 100000 {
		writeError(w, http.StatusBadRequest, errors.New("monthlySpendLimitCents must be a whole number of cents between 1 and 100000"))
		return
	}
	if settings.RetainFailedMinutes < 0 || settings.RetainFailedMinutes > 30 {
		writeError(w, http.StatusBadRequest, errors.New("retainFailedMinutes must be between 0 and 30"))
		return
	}
	size, ok := pricing.FindCI(settings.DefaultSize)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("defaultSize must be a supported CI runner size"))
		return
	}
	settings.DefaultSize = size.ID
	if settings.PaidOverrunEnabled {
		writeError(w, http.StatusConflict, errors.New("Paid overrun is not yet available; metering and runner runtime are not activated."))
		return
	}
	if settings.RetainFailedMinutes != 0 {
		writeError(w, http.StatusConflict, errors.New("Failed-run retention is not yet available."))
		return
	}
	if err := h.service.Store.SaveCISettings(r.Context(), workspaceID, p.Account.ID, settings); err != nil {
		writeStoreError(w, err)
		return
	}
	_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "ci.settings.updated", workspaceID, map[string]any{"defaultSize": settings.DefaultSize, "cacheEnabled": settings.CacheEnabled})
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

func (h *HTTPServer) ciJobCancel(w http.ResponseWriter, r *http.Request, p Principal, workspaceID, jobID string) {
	if p.Account != nil && p.Installation == nil {
		role, err := workspaceRole(r.Context(), h.service.Store.pool, workspaceID, p.Account.ID, false)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if role != "owner" && role != "operator" {
			writeStoreError(w, ErrForbidden)
			return
		}
	} else if p.Installation != nil {
		if !p.Installation.Authority.Draft {
			writeStoreError(w, ErrForbidden)
			return
		}
	} else {
		writeStoreError(w, ErrForbidden)
		return
	}
	row, _, err := h.service.Store.CIJobRow(r.Context(), workspaceID, jobID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	visible, err := h.ciJobVisible(r.Context(), p, workspaceID, row)
	if errors.Is(err, errCIRepoUnavailable) {
		writeError(w, http.StatusServiceUnavailable, errors.New("Repository access could not be verified."))
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !visible {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	job, changed, err := h.service.Store.CancelCIJob(r.Context(), workspaceID, jobID)
	if errors.Is(err, ErrCICancelActive) {
		writeError(w, http.StatusConflict, ErrCICancelActive)
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if changed {
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "ci.job.cancelled", jobID, map[string]any{"status": job.Status})
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

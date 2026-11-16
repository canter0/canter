package remote

import (
	"context"
	"encoding/json"
	"net/http"
)

// InspectCI returns the raw workspace CI overview document exactly as the
// control plane emitted it. Callers render or interpret the fields; the
// client deliberately invents no diagnostics or derived metrics.
func (c *Client) InspectCI(ctx context.Context, accessToken, workspaceID string) (json.RawMessage, error) {
	var result json.RawMessage
	escapedWorkspaceID, err := escapePathID("workspace ID", workspaceID)
	if err != nil {
		return nil, err
	}
	err = c.do(ctx, http.MethodGet, "/v1/workspaces/"+escapedWorkspaceID+"/ci", accessToken, nil, &result)
	return result, err
}

// InspectCIJob returns the raw CI job detail document (job, steps, logs,
// metrics, tests) exactly as the control plane emitted it.
func (c *Client) InspectCIJob(ctx context.Context, accessToken, workspaceID, jobID string) (json.RawMessage, error) {
	var result json.RawMessage
	escapedWorkspaceID, err := escapePathID("workspace ID", workspaceID)
	if err != nil {
		return nil, err
	}
	escapedJobID, err := escapePathID("job ID", jobID)
	if err != nil {
		return nil, err
	}
	err = c.do(ctx, http.MethodGet, "/v1/workspaces/"+escapedWorkspaceID+"/ci/jobs/"+escapedJobID, accessToken, nil, &result)
	return result, err
}

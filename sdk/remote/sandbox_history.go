package remote

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type SandboxHistoryJob struct {
	JobID         string     `json:"jobId"`
	Generation    *int64     `json:"generation,omitempty"`
	Kind          string     `json:"kind"`
	ActorKind     string     `json:"actorKind,omitempty"`
	ActorID       string     `json:"actorId,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
	ObservedAt    *time.Time `json:"observedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Status        string     `json:"status"`
	ExitCode      *int       `json:"exitCode,omitempty"`
	OutputState   string     `json:"outputState"`
	RetainedBytes int        `json:"retainedBytes"`
	Stale         bool       `json:"stale"`
}

type SandboxHistory struct {
	Jobs             []SandboxHistoryJob `json:"jobs"`
	More             bool                `json:"more"`
	CollectorEnabled bool                `json:"collectorEnabled"`
	RetentionEnabled bool                `json:"retentionEnabled"`
	RetentionDays    int                 `json:"retentionDays"`
	JobByteLimit     int                 `json:"jobByteLimit"`
}

type SandboxHistoryOutput struct {
	Job        SandboxHistoryJob `json:"job"`
	Data       []byte            `json:"dataBase64"`
	NextOffset int64             `json:"nextOffset"`
}

func (s *SandboxClient) History(ctx context.Context, id string) (SandboxHistory, error) {
	var out SandboxHistory
	path, err := s.sandboxPath(id, "/history")
	if err != nil {
		return out, err
	}
	err = s.client.do(ctx, http.MethodGet, path, s.accessToken, nil, &out)
	return out, err
}

func (s *SandboxClient) HistoryOutput(ctx context.Context, id, job string, offset int64) (SandboxHistoryOutput, error) {
	var out SandboxHistoryOutput
	escaped, err := sandboxSegment("job ID", job)
	if err != nil {
		return out, err
	}
	path, err := s.sandboxPath(id, "/history/"+escaped+"?offset="+strconv.FormatInt(offset, 10))
	if err != nil {
		return out, err
	}
	err = s.client.do(ctx, http.MethodGet, path, s.accessToken, nil, &out)
	return out, err
}

package controlplane

import "time"

const (
	ciCacheMaxOperation    = 120 * time.Second
	ciCacheCleanupLease    = 3 * time.Minute
	ciCacheCleanupRetry    = 5 * time.Second
	ciCacheCleanupWorkers  = 2
	ciCacheReadWorkers     = 4
	ciCacheUploadWorkers   = 2
	ciCacheMaxKeyBytes     = 512
	ciCacheMaxVersionBytes = 128
	ciCacheMaxObjects      = 10000
	ciCacheMaxRetention    = 7 * 24 * time.Hour
)

type ciCachePoolConfig struct {
	BackendDigest       string
	Enabled             bool
	MaxTotalBytes       int64
	MaxWorkspaceBytes   int64
	MaxEntryBytes       int64
	MaxObjects          int64
	MaxWorkspaceObjects int64
	TTLSeconds          int64
}

type ciCachePoolState struct {
	ciCachePoolConfig
	ReservedBytes   int64
	ReservedObjects int64
}

// Only the server constructs a grant, after signature verification, durable
// admitted-job lookup and GitHub's exact check-run binding. It is not JSON.
type ciCacheGrant struct {
	job       ciCacheActiveJob
	token     ciCacheToken
	identity  ciCacheIdentity
	namespace string
}

type ciCacheObject struct {
	ID               string
	SizeBytes        int64
	SHA256           string
	State            string
	WriterToken      string
	UploadDeadlineAt time.Time
	ExpiresAt        time.Time
	KeyDigest        string
	VersionDigest    string
}

type ciCacheEntry struct {
	ObjectID      string
	BackendDigest string
	SizeBytes     int64
	SHA256        string
	ExpiresAt     time.Time
}

type ciCacheCleanupClaim struct {
	ObjectID      string
	WorkspaceID   string
	SizeBytes     int64
	BackendDigest string
	CleanupToken  string
}

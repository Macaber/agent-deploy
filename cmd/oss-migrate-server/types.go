package main

import (
	"encoding/json"
	"time"
)

// Default configuration constants.
const (
	DefaultSourceBucket = "oss://euvduatc-oss"
	DefaultSourcePrefix = "workspaces"
	DefaultTargetBucket = "oss://euvduatc-oss"
	DefaultTargetPrefix = "workspaces"
	DefaultDirs         = "workspace,bocomwork,bocomcode"
	DefaultExcludes     = "" // default empty: migrate everything without excludes
	DefaultRetries      = 3
	DefaultRetrySleep   = 5 * time.Second
	DefaultLogFile      = "/var/log/oss-migrate-server/oss2oss-migrate.log"
)

// MigrateConfig holds the resolved global settings for the OSS-to-OSS migrator.
type MigrateConfig struct {
	SourceBucket string        `json:"sourceBucket"` // default oss://euvduatc-oss
	SourcePrefix string        `json:"sourcePrefix"` // default workspaces
	TargetBucket string        `json:"targetBucket"` // default oss://euvduatc-oss
	TargetPrefix string        `json:"targetPrefix"` // default workspaces
	Dirs         []string      `json:"dirs"`         // default workspace,bocomwork,bocomcode
	Excludes     []string      `json:"excludes"`     // default bocomwork/server/skills/builtin
	Retries      int           `json:"retries"`
	RetrySleep   time.Duration `json:"retrySleep"`
	LogFile      string        `json:"logFile"`
	OSSUtilPath  string        `json:"ossUtilPath"`
	ExtraArgs    []string      `json:"extraArgs"`
}

// MigrationItem defines a single OSS to OSS migration pair.
type MigrationItem struct {
	Source      string   `json:"source"`             // e.g. oss://euvduatc-oss/workspaces/ws-9743616
	Destination string   `json:"destination"`        // e.g. oss://euvduatc-oss/workspaces/ws-974361-guwnb
	Force       bool     `json:"force"`              // overwrite if target exists
	DryRun      bool     `json:"dryRun"`             // preview only, do not copy
	Dirs        []string `json:"dirs,omitempty"`     // specific subdirs to copy; if empty, copies whole prefix
	Excludes    []string `json:"excludes,omitempty"` // patterns to exclude
}

// MigrateResult holds the outcome of a single MigrationItem.
type MigrateResult struct {
	Source        string   `json:"source"`
	Destination   string   `json:"destination"`
	Status        string   `json:"status"` // OK | SKIP | FAIL
	SourceObjects int64    `json:"sourceObjects"`
	TargetObjects int64    `json:"targetObjects"`
	Size          string   `json:"size"`
	Note          string   `json:"note"`
	Elapsed       string   `json:"elapsed"`
	Synced        []string `json:"synced,omitempty"`
	Skipped       []string `json:"skipped,omitempty"`
	Missing       []string `json:"missing,omitempty"`
	Details       []string `json:"details,omitempty"`
}

// RemoteCheckRequest is the payload for probing target OSS prefixes.
type RemoteCheckRequest struct {
	URIs []string `json:"uris"`
}

// RemoteCheckResult is the probe response for a single OSS URI.
type RemoteCheckResult struct {
	URI         string `json:"uri"`
	Exists      bool   `json:"exists"`
	ObjectCount int64  `json:"objectCount"`
	Error       string `json:"error,omitempty"`
}

// OSSListRequest is the request to list workspaces / prefixes under an OSS path.
type OSSListRequest struct {
	Bucket string `json:"bucket"` // e.g. oss://euvduatc-oss
	Prefix string `json:"prefix"` // e.g. workspaces
}

// OSSWorkspaceEntry represents a discovered workspace in OSS.
type OSSWorkspaceEntry struct {
	Name          string `json:"name"`          // e.g. ws-9743616
	SourceURI     string `json:"sourceURI"`     // e.g. oss://euvduatc-oss/workspaces/ws-9743616
	TargetURI     string `json:"targetURI"`     // e.g. oss://euvduatc-oss/workspaces/ws-9743616
	TargetExists  bool   `json:"targetExists"`  // whether target already has data
	TargetObjects int64  `json:"targetObjects"` // object count in target
}

// MarshalJSON outputs both lowercase ("name", "sourceURI") and PascalCase ("Name", "SourceURI")
// to guarantee frontend compatibility and eliminate undefined field errors.
func (e OSSWorkspaceEntry) MarshalJSON() ([]byte, error) {
	type Alias OSSWorkspaceEntry
	return json.Marshal(&struct {
		Alias
		NameUpper      string `json:"Name"`
		SourceURIUpper string `json:"SourceURI"`
		TargetURIUpper string `json:"TargetURI"`
	}{
		Alias:          Alias(e),
		NameUpper:      e.Name,
		SourceURIUpper: e.SourceURI,
		TargetURIUpper: e.TargetURI,
	})
}

// RunMigrationRequest is the payload for /api/migrate/run.
type RunMigrationRequest struct {
	Tasks []MigrationItem `json:"tasks"`

	// Global overrides if not set on individual items:
	Force    bool     `json:"force"`
	DryRun   bool     `json:"dryRun"`
	Dirs     []string `json:"dirs,omitempty"`
	Excludes []string `json:"excludes,omitempty"`
}

// LogRow represents a single line parsed from the TSV log file.
type LogRow struct {
	Timestamp   string `json:"timestamp"`
	Status      string `json:"status"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Dirs        string `json:"dirs"`
	Size        string `json:"size"`
	Files       string `json:"files"`
	Elapsed     string `json:"elapsed"`
	Note        string `json:"note"`
}

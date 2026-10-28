// Package driver is the builder's view of the engine: the operations a
// build needs (inspect, pull, load, tag, create, archive in and out, run,
// commit), and Client, which performs them over the engine's API — the
// subset of the Docker Engine API that wctrd serves, plus the endpoints a
// builder needs from it (commit, tag, load, container archives).
package driver

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound reports a missing image or container.
var ErrNotFound = errors.New("not found")

// HealthConfig is an image's HEALTHCHECK (Docker's image config field).
// Durations are nanoseconds, as Docker stores them.
type HealthConfig struct {
	Test          []string `json:"Test,omitempty"`
	Interval      int64    `json:"Interval,omitempty"`
	Timeout       int64    `json:"Timeout,omitempty"`
	StartPeriod   int64    `json:"StartPeriod,omitempty"`
	StartInterval int64    `json:"StartInterval,omitempty"`
	Retries       int      `json:"Retries,omitempty"`
}

// ImageConfig is the "config" object of an image config: the OCI fields
// plus Docker's Shell, OnBuild and Healthcheck.
type ImageConfig struct {
	User         string              `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	Shell        []string            `json:"Shell,omitempty"`
	OnBuild      []string            `json:"OnBuild,omitempty"`
	Healthcheck  *HealthConfig       `json:"Healthcheck,omitempty"`
}

// Image is what the builder needs to know of an image.
type Image struct {
	ID           string
	OS           string
	Architecture string
	Author       string
	Config       ImageConfig
	// Layers is the number of layers (bounded by the engine).
	Layers int
}

// RunSpec makes a container for one build step.
type RunSpec struct {
	Image string
	// Entrypoint is the command; the image's Cmd is dropped.
	Entrypoint []string
	// Env is added to the image's (same key: this wins).
	Env        []string
	WorkingDir string
	User       string
	// Network: "" (the engine's default) or "none".
	Network string
}

// CommitSpec makes an image of a container.
type CommitSpec struct {
	Container string
	// Repo and Tag name the image; empty leaves it addressable by ID only.
	Repo, Tag string
	Comment   string
	Author    string
	// Config replaces the container's configuration in the image.
	Config ImageConfig
}

// Engine is what a build drives.
type Engine interface {
	// InspectImage returns ErrNotFound (wrapped) for a missing image.
	InspectImage(ctx context.Context, ref string) (*Image, error)
	// PullImage fetches ref for platform wasip1/wasm.
	PullImage(ctx context.Context, ref string, progress io.Writer) error
	// LoadImage imports an OCI image layout sent as a tar and returns the
	// ID of the image it named (or of its only image).
	LoadImage(ctx context.Context, layout io.Reader) (string, error)
	TagImage(ctx context.Context, image, repo, tag string) error
	CreateContainer(ctx context.Context, spec RunSpec) (string, error)
	// PutArchive extracts a tar into the container at dir.
	PutArchive(ctx context.Context, id, dir string, tar io.Reader) error
	// StatPath reports whether path exists in the container (HEAD on the
	// archive endpoint: no content is sent).
	StatPath(ctx context.Context, id, path string) (bool, error)
	// GetArchive returns a tar of path, its base name as the root entry.
	GetArchive(ctx context.Context, id, path string) (io.ReadCloser, error)
	// Run starts the container, copies its output, and returns its exit
	// status once it exits. A module that cannot be started is reported
	// as its exit status (127 missing, 126 not executable) with the
	// engine's message in err of type *ExitError.
	Run(ctx context.Context, id string, stdout, stderr io.Writer) (int, error)
	Commit(ctx context.Context, spec CommitSpec) (string, error)
	RemoveContainer(ctx context.Context, id string) error
}

// Exit statuses the engine uses (engine/runtime).
const (
	ExitNotExecutable = 126
	ExitMissing       = 127
)

// ExitError is a step that could not run.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

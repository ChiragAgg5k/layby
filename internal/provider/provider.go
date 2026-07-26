package provider

import (
	"context"
	"errors"
	"io"
	"time"
)

// Provider is the whole surface a backend must implement. It is deliberately
// five methods: anything richer belongs in Capabilities, so a weak provider
// degrades visibly instead of forcing every other driver to grow.
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Create(ctx context.Context, spec Specification) (Handle, error)
	Status(ctx context.Context, handle Handle) (State, error)
	Execute(ctx context.Context, handle Handle, command []string, output io.Writer) (int, error)
	Shell(ctx context.Context, handle Handle) error
	Destroy(ctx context.Context, handle Handle) error
	List(ctx context.Context) ([]Handle, error)
}

// Capabilities is what a driver advertises about itself. The CLI reads these
// to decide what to offer and what to warn about, rather than special-casing
// provider names at the call site.
type Capabilities struct {
	Snapshot              bool
	Fork                  bool
	PersistentDisk        bool
	WarmPool              bool
	SubMinuteBoot         bool
	PerSandboxCredentials bool
	InteractiveShell      bool
}

// Specification is the normalized creation request. Drivers map Size and
// Region onto whatever their backend calls those things.
type Specification struct {
	Identifier  string
	Image       string
	Size        string
	Region      string
	Environment map[string]string
	TimeToLive  time.Duration
	Labels      map[string]string
}

// Handle identifies a running sandbox. Reference is the driver's own opaque
// identifier; Identifier is ours and is what the user types.
type Handle struct {
	Identifier string
	Provider   string
	Reference  string
	Image      string
	Size       string
	Region     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// State is the lifecycle position of a sandbox as the provider sees it.
type State string

const (
	StatePending  State = "pending"
	StateReady    State = "ready"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	StateNotFound State = "not-found"
)

// ErrNotFound means the provider has no record of the handle. Reconciliation
// treats this as authoritative: the sandbox is gone, drop the local record.
var ErrNotFound = errors.New("sandbox not found")

// Expired reports whether the sandbox has outlived its TTL. This is the
// client-side half of expiry enforcement; the sandbox also self-destructs from
// the inside so a closed laptop cannot leak a paid instance.
func (h Handle) Expired(now time.Time) bool {
	return !h.ExpiresAt.IsZero() && now.After(h.ExpiresAt)
}

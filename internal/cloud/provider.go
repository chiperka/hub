// Package cloud runs Chiperka workers on demand on cloud servers (Hetzner Cloud
// or AWS EC2): it watches the hub's job queue, rents servers when the queue is
// long, connects them to the hub over a reverse SSH tunnel opened by the hub,
// and returns them once the queue is drained.
package cloud

import (
	"context"
	"errors"
	"time"
)

// Labels put on every resource the hub creates. LabelManaged lets the janitor
// find leftovers even when the hub is gone; LabelHub keeps two hubs sharing one
// cloud project from deleting each other's servers.
const (
	LabelManaged = "chiperka-managed"
	LabelHub     = "chiperka-hub"
	LabelImage   = "chiperka-image" // value "worker" on prepared server images
	ImageKind    = "worker"
)

// Instance states normalised across providers.
const (
	StatePending    = "pending"
	StateRunning    = "running"
	StateStopped    = "stopped"
	StateTerminated = "terminated"
)

var (
	// ErrUnavailable means the provider has no capacity for this server type in
	// this location right now. The autoscaler tries the next type/location.
	ErrUnavailable = errors.New("server type not available")
	// ErrQuota means an account limit was hit. The autoscaler backs off.
	ErrQuota = errors.New("provider quota exceeded")
	// ErrNotConfigured means the provider has no credentials.
	ErrNotConfigured = errors.New("provider not configured")
)

// Provider is a cloud the hub can rent worker servers from.
type Provider interface {
	Name() string     // "hetzner" or "aws"
	Currency() string // "EUR" or "USD"

	// Check verifies the credentials with a cheap read-only call.
	Check(ctx context.Context) error
	// Prepare makes sure the shared resources exist and are up to date:
	// the firewall / security group allowing SSH only from allowCIDR, and on
	// Hetzner the hub's SSH key (so no root password is ever e-mailed).
	Prepare(ctx context.Context, p PrepareRequest) error

	Create(ctx context.Context, r CreateRequest) (*Instance, error)
	Delete(ctx context.Context, id string) error
	// List returns every instance labelled LabelManaged=true, whatever its hub.
	List(ctx context.Context) ([]Instance, error)

	// HourlyPrice is the net price of one server per hour in Currency().
	HourlyPrice(ctx context.Context, serverType, location string, spot bool) (float64, error)
	// ValidateType reports whether the provider offers the type at all.
	ValidateType(ctx context.Context, serverType string) error

	// Server images.
	BaseImage(ctx context.Context) (string, error) // stock Ubuntu 24.04 to build an image from
	LatestImage(ctx context.Context) (*Image, error)
	ListImages(ctx context.Context) ([]Image, error)
	// CreateImage snapshots a stopped instance and waits until the image is usable.
	CreateImage(ctx context.Context, instanceID, name string) (*Image, error)
	DeleteImage(ctx context.Context, id string) error
}

type PrepareRequest struct {
	HubID        string
	SSHAllowCIDR string // e.g. "203.0.113.7/32"
	HubPublicKey string // authorized_keys format
}

type CreateRequest struct {
	Name     string
	Type     string
	Location string // Hetzner location (nbg1) or AWS availability zone (eu-central-1a)
	Image    string // provider image ID; empty = LatestImage, else BaseImage
	UserData string // cloud-init
	DiskGB   int
	Labels   map[string]string
	// StopOnShutdown keeps a powered-off server (image builds snapshot it).
	// Workers leave it false: on AWS a shutdown then terminates the server.
	StopOnShutdown bool
	// AWS only.
	Spot         bool
	SpotFallback bool // when spot capacity is missing, launch on-demand instead
}

type Instance struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Location    string            `json:"location"`
	PublicIP    string            `json:"public_ip"`
	State       string            `json:"state"`
	StateReason string            `json:"state_reason,omitempty"` // e.g. spot interruption
	Spot        bool              `json:"spot"`
	CreatedAt   time.Time         `json:"created_at"`
	Labels      map[string]string `json:"labels"`
}

// SpotInterrupted reports whether AWS took the instance back.
func (i *Instance) SpotInterrupted() bool {
	return i.Spot && i.State == StateTerminated && i.StateReason == "Server.SpotInstanceTermination"
}

type Image struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	SizeGB    float64   `json:"size_gb"`
}

// Labels every server of this hub carries.
func managedLabels(hubID string) map[string]string {
	return map[string]string{LabelManaged: "true", LabelHub: hubID}
}

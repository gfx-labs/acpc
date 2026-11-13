package acpc

import (
	"github.com/gfx-labs/acphost/pkg/acphost"
	"github.com/gfx-labs/acphost/pkg/cgroup"
	"github.com/gfx-labs/acphost/pkg/sandbox"
	"github.com/gfx-labs/acphost/pkg/userns"
)

// Config is a session's isolation: the sections of a jobshop agent or
// pool, with the same keys as acphost's JSON (sandbox.Sandbox,
// userns.User, cgroup.Cgroup). Unknown keys are an error.
type Config struct {
	Sandbox *sandbox.Sandbox `json:"sandbox,omitempty"`
	User    *userns.User     `json:"user,omitempty"`
	Cgroup  *cgroup.Cgroup   `json:"cgroup,omitempty"`
	// Env is KEY=VALUE added to the command's environment.
	Env []string `json:"env,omitempty"`
	// Join maps a namespace type to a namespace file (Spec.Join).
	Join map[string]string `json:"join,omitempty"`
	// Listen and TLS are Spec.Listen and Spec.TLS: the optional TCP
	// listener of a host.
	Listen string             `json:"listen,omitempty"`
	TLS    *acphost.ListenTLS `json:"tls,omitempty"`
}

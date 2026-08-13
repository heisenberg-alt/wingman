package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Device is one registered push target.
type Device struct {
	Token   string    `json:"token"`
	Env     string    `json:"env"` // "production" | "sandbox"
	Name    string    `json:"name,omitempty"`
	AddedAt time.Time `json:"addedAt"`
}

// Registry is the persistent set of device push tokens.
type Registry struct {
	path string

	mu      sync.Mutex
	devices []Device
}

// LoadTokens reads the token registry at path, returning an empty registry
// if the file does not exist yet.
func LoadTokens(path string) (*Registry, error) {
	r := &Registry{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.devices); err != nil {
		return nil, fmt.Errorf("push: parse token registry %s: %w", path, err)
	}
	return r, nil
}

// Register upserts a device token and persists the registry.
func (r *Registry) Register(token, env, name string) error {
	if token == "" {
		return errors.New("push: empty device token")
	}
	if env != EnvProduction && env != EnvSandbox {
		return fmt.Errorf("push: unknown APNs environment %q", env)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, d := range r.devices {
		if d.Token == token {
			r.devices[i].Env = env
			r.devices[i].Name = name
			return r.save()
		}
	}
	r.devices = append(r.devices, Device{Token: token, Env: env, Name: name, AddedAt: time.Now().UTC()})
	return r.save()
}

// Remove drops a token (e.g. after APNs reports it gone) and persists.
func (r *Registry) Remove(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, d := range r.devices {
		if d.Token == token {
			r.devices = append(r.devices[:i], r.devices[i+1:]...)
			_ = r.save()
			return
		}
	}
}

// List returns a snapshot of registered devices.
func (r *Registry) List() []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Device, len(r.devices))
	copy(out, r.devices)
	return out
}

// save persists the registry; the caller holds r.mu.
func (r *Registry) save() error {
	data, err := json.MarshalIndent(r.devices, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o600)
}

// Notifier fans permission-request alerts out to every registered device.
type Notifier struct {
	APNS   *APNS
	Tokens *Registry
	Logger *slog.Logger
}

// PermissionRequested pushes a time-sensitive approval alert. The payload
// carries only the title and routing ids — tool-call contents stay inside
// the encrypted channel (ADR-0006). Delivery is best-effort: failures are
// logged, and tokens APNs reports gone are dropped.
func (n *Notifier) PermissionRequested(sessionID, requestID, title string, optionIDs []string) {
	if title == "" {
		title = "Copilot needs permission"
	}
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": "Approval needed",
				"body":  title,
			},
			"sound":              "default",
			"interruption-level": "time-sensitive",
			"category":           "WINGMAN_APPROVAL",
			"thread-id":          sessionID,
		},
		"sessionId": sessionID,
		"requestId": requestID,
		"options":   optionIDs,
	})

	for _, d := range n.Tokens.List() {
		go func(d Device) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := n.APNS.Push(ctx, d.Token, d.Env, payload)
			switch {
			case errors.Is(err, ErrTokenGone):
				n.Logger.Info("dropping gone push token", "device", d.Name)
				n.Tokens.Remove(d.Token)
			case err != nil:
				n.Logger.Warn("push failed", "device", d.Name, "err", err)
			}
		}(d)
	}
}

// Package sysinfo reads the two things the page shows of the system: which backup targets use
// which account (so an account in use is not removed unseen, and its card can say so), and whether
// backup encryption is set up (a cloud target cannot be made without it). It asks the system's API
// with the addon's own token (scope system:read) and keeps the answer a few seconds.
package sysinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Target is a backup target of the system that uses one of the addon's accounts.
type Target struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Facts is what the page shows of the system.
type Facts struct {
	// Reachable: the system answered with the addon's token.
	Reachable bool `json:"reachable"`
	// Provider: the system knows this addon as a provider of backup targets (an older system does not).
	Provider bool `json:"provider"`
	// Encryption: a recovery key is set up.
	Encryption bool `json:"encryption"`
	// Targets are the backup targets by the account they use.
	Targets map[string][]Target `json:"targets"`
	Error   string              `json:"error,omitempty"`
}

// Client asks the system.
type Client struct {
	// Base is the system's URL (http://127.0.0.1).
	Base string
	HTTP *http.Client
	// AddonID is this addon's id.
	AddonID string

	mu   sync.Mutex
	at   time.Time
	last Facts
}

// Facts answers the facts, at most 5 s old unless fresh is asked for.
func (c *Client) Facts(ctx context.Context, fresh bool) Facts {
	c.mu.Lock()
	if !fresh && time.Since(c.at) < 5*time.Second {
		f := c.last
		c.mu.Unlock()
		return f
	}
	c.mu.Unlock()
	f := c.read(ctx)
	c.mu.Lock()
	c.at, c.last = time.Now(), f
	c.mu.Unlock()
	return f
}

func (c *Client) read(ctx context.Context) Facts {
	f := Facts{Targets: map[string][]Target{}}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/system/v1/backup/targets", nil)
	res, err := c.HTTP.Do(req)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		f.Error = fmt.Sprintf("the system answered HTTP %d", res.StatusCode)
		return f
	}
	var v struct {
		Encryption bool `json:"encryption"`
		Providers  []struct {
			Addon string `json:"addon"`
		} `json:"providers"`
		Targets []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Addon *struct {
				ID          string `json:"id"`
				Destination string `json:"destination"`
			} `json:"addon"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		f.Error = "the system's answer could not be read"
		return f
	}
	f.Reachable, f.Encryption = true, v.Encryption
	for _, p := range v.Providers {
		if p.Addon == c.AddonID {
			f.Provider = true
		}
	}
	for _, t := range v.Targets {
		if t.Kind == "addon" && t.Addon != nil && t.Addon.ID == c.AddonID {
			f.Targets[t.Addon.Destination] = append(f.Targets[t.Addon.Destination], Target{ID: t.ID, Name: t.Name})
		}
	}
	return f
}

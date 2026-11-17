// Package systimers is the addon's side of the system's "an addon's own timers" (openccu-lite's
// scope timers:addon): each copy job is one timer local-sync-<job> on the system, made, switched,
// run and removed here with the addon's own API token. The addon hands over the schedule - the
// timer's text - and the system renders the service that runs `lite-sync run <job>` as the addon's
// user. It also reads when the timers run next (the scope system:read).
package systimers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Timer is one of the addon's timers as the system has it.
type Timer struct {
	Name        string   `json:"name"`
	Unit        string   `json:"unit"`
	Service     string   `json:"service"`
	TimerFile   string   `json:"timer_file"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	Read        []string `json:"read"`
}

// Input is what the addon sends to make or change a timer; a nil field stays as it is.
type Input struct {
	Timer       string    `json:"timer,omitempty"`
	Description *string   `json:"description,omitempty"`
	Read        *[]string `json:"read,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

// Times is when a timer ran last and runs next (RFC 3339, "" for never / not scheduled).
type Times struct {
	Next string `json:"next,omitempty"`
	Last string `json:"last,omitempty"`
}

// Error is the system's refusal.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the system answered HTTP %d", e.Status)
}

// ErrUnsupported: this system does not offer an addon timers (it is older than the addon needs),
// or the addon's token does not hold the scope.
var ErrUnsupported = errors.New("this system does not offer timers to addons")

// IsNotFound says whether err is the system's "no such timer".
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Client talks to the system's API.
type Client struct {
	// Base is the system's URL (http://127.0.0.1); HTTP carries the addon's token.
	Base string
	HTTP *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/system/v1"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Occulite-Request", "1")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		e := &Error{Status: res.StatusCode}
		var v struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &v) == nil {
			e.Code, e.Message = v.Error, v.Message
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// List answers the addon's timers and the addons whose data a timer may read. ErrUnsupported on a
// system without the routes (404) or without the scope for this addon (403, 501).
func (c *Client) List(ctx context.Context) ([]Timer, []string, error) {
	var v struct {
		Timers   []Timer  `json:"timers"`
		Readable []string `json:"readable"`
	}
	if err := c.do(ctx, http.MethodGet, "/addon-timers", nil, &v); err != nil {
		var e *Error
		if errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Status == http.StatusForbidden || e.Status == http.StatusNotImplemented || e.Status == http.StatusUnauthorized) {
			return nil, nil, fmt.Errorf("%w (HTTP %d %s)", ErrUnsupported, e.Status, e.Code)
		}
		return nil, nil, err
	}
	return v.Timers, v.Readable, nil
}

// Put makes or changes the timer of a job.
func (c *Client) Put(ctx context.Context, name string, in Input) (Timer, error) {
	var t Timer
	err := c.do(ctx, http.MethodPut, "/addon-timers/"+url.PathEscape(name), in, &t)
	return t, err
}

// Delete removes a job's timer; one that is not there is no error.
func (c *Client) Delete(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/addon-timers/"+url.PathEscape(name), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) act(ctx context.Context, name, what string) error {
	return c.do(ctx, http.MethodPost, "/addon-timers/"+url.PathEscape(name)+"/"+what, nil, nil)
}

// Run starts a job's service now, without waiting for it.
func (c *Client) Run(ctx context.Context, name string) error { return c.act(ctx, name, "run") }

// Stop stops a job's running service.
func (c *Client) Stop(ctx context.Context, name string) error { return c.act(ctx, name, "stop") }

// SetEnabled switches a job's timer; off keeps its files.
func (c *Client) SetEnabled(ctx context.Context, name string, on bool) error {
	if on {
		return c.act(ctx, name, "enable")
	}
	return c.act(ctx, name, "disable")
}

// Times answers every timer's next and last run by its unit's name; an empty map when the system
// does not say.
func (c *Client) Times(ctx context.Context) map[string]Times {
	var v struct {
		Timers []struct {
			Unit string `json:"unit"`
			Next string `json:"next"`
			Last string `json:"last"`
		} `json:"timers"`
	}
	out := map[string]Times{}
	if err := c.do(ctx, http.MethodGet, "/timers", nil, &v); err != nil {
		return out
	}
	for _, t := range v.Timers {
		out[t.Unit] = Times{Next: t.Next, Last: t.Last}
	}
	return out
}

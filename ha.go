package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/host"
)

// errGone means Home Assistant deleted this computer's registration.
var errGone = errors.New("registration deleted in Home Assistant")

// Sensor is one reading in the mobile_app webhook format. Name and the fields
// after it are only sent when the sensor is registered.
type Sensor struct {
	UniqueID       string         `json:"unique_id"`
	Type           string         `json:"type"`
	State          any            `json:"state"`
	Icon           string         `json:"icon,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	Name           string         `json:"name,omitempty"`
	DeviceClass    string         `json:"device_class,omitempty"`
	Unit           string         `json:"unit_of_measurement,omitempty"`
	StateClass     string         `json:"state_class,omitempty"`
	EntityCategory string         `json:"entity_category,omitempty"`
}

type update struct {
	UniqueID   string         `json:"unique_id"`
	Type       string         `json:"type"`
	State      any            `json:"state"`
	Icon       string         `json:"icon,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type client struct {
	url, webhook string
	http         *http.Client
	registered   map[string]bool
}

func newClient(url, webhook string) *client {
	return &client{url: url, webhook: webhook, http: &http.Client{Timeout: 20 * time.Second}, registered: map[string]bool{}}
}

func (c *client) post(ctx context.Context, payload any) ([]byte, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/webhook/"+c.webhook, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The error repeats the URL, and the webhook ID in it is a secret: keep
		// it out of the log and the menu.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = c.url + "/api/webhook/…"
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusGone:
		return nil, errGone
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("Home Assistant answered %s", resp.Status)
	}
	return data, nil
}

func (c *client) register(ctx context.Context, s Sensor) error {
	data, err := c.post(ctx, map[string]any{"type": "register_sensor", "data": s})
	if err != nil {
		return err
	}
	var res struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(data, &res) != nil || !res.Success {
		return fmt.Errorf("registering %s: unexpected answer %q", s.UniqueID, strings.TrimSpace(string(data)))
	}
	c.registered[s.UniqueID] = true
	return nil
}

// send registers sensors it hasn't registered yet, then updates them all.
// Sensors Home Assistant says it doesn't know are registered again next time.
func (c *client) send(ctx context.Context, sensors []Sensor) error {
	for _, s := range sensors {
		if !c.registered[s.UniqueID] {
			if err := c.register(ctx, s); err != nil {
				return err
			}
		}
	}
	updates := make([]update, len(sensors))
	for i, s := range sensors {
		updates[i] = update{UniqueID: s.UniqueID, Type: s.Type, State: s.State, Icon: s.Icon, Attributes: s.Attributes}
	}
	data, err := c.post(ctx, map[string]any{"type": "update_sensor_states", "data": updates})
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("Home Assistant gave no answer; the registration may have been deleted")
	}
	var res map[string]struct {
		Success bool `json:"success"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("unexpected answer %q", strings.TrimSpace(string(data)))
	}
	var problems []string
	for id, r := range res {
		if r.Success {
			continue
		}
		if r.Error.Code == "not_registered" {
			delete(c.registered, id)
		}
		problems = append(problems, fmt.Sprintf("%s: %s", id, r.Error.Code))
	}
	if len(problems) > 0 {
		return fmt.Errorf("Home Assistant rejected %s", strings.Join(problems, ", "))
	}
	return nil
}

// registerDevice adds this computer to Home Assistant as a mobile_app device and
// returns its webhook ID. This is the only call that needs the access token.
func registerDevice(ctx context.Context, url, token, name string) (string, error) {
	info, _ := host.InfoWithContext(ctx)
	osName, osVersion, manufacturer := runtime.GOOS, "", ""
	if info != nil {
		osVersion = info.PlatformVersion
	}
	switch runtime.GOOS {
	case "darwin":
		osName, manufacturer = "macOS", "Apple"
	case "windows":
		osName, manufacturer = "Windows", "Microsoft"
	case "linux":
		osName, manufacturer = "Linux", "Linux"
		if info != nil && info.Platform != "" {
			osName = info.Platform
		}
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	hostname, _ := os.Hostname()
	payload := map[string]any{
		"device_id":           "familiar-" + hex.EncodeToString(id),
		"app_id":              "zone.olivia.familiar",
		"app_name":            "Familiar",
		"app_version":         version,
		"device_name":         name,
		"manufacturer":        manufacturer,
		"model":               hostname,
		"os_name":             osName,
		"os_version":          osVersion,
		"supports_encryption": false,
		"app_data":            map[string]any{},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/mobile_app/registrations", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("Home Assistant refused the token")
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("registering answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var res struct {
		WebhookID string `json:"webhook_id"`
	}
	if json.Unmarshal(data, &res) != nil || res.WebhookID == "" {
		return "", fmt.Errorf("registering gave no webhook ID: %s", strings.TrimSpace(string(data)))
	}
	return res.WebhookID, nil
}

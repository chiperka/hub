package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hetzner talks to the Hetzner Cloud API. The token should belong to a project
// used only by Chiperka: the API cannot restrict a token any further.
type Hetzner struct {
	Token   string
	BaseURL string // tests override it
	HTTP    *http.Client
}

func NewHetzner(token string) *Hetzner {
	return &Hetzner{Token: token, BaseURL: "https://api.hetzner.cloud/v1", HTTP: &http.Client{Timeout: 60 * time.Second}}
}

func (h *Hetzner) Name() string     { return "hetzner" }
func (h *Hetzner) Currency() string { return "EUR" }

const hetznerResourceName = "chiperka-autoscaler"

// firewallName is per hub: hubs (and the image builder) sharing a project must
// not overwrite each other's SSH rule.
func firewallName(hubID string) string { return hetznerResourceName + "-" + hubID }

type hcError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *hcError) Error() string { return fmt.Sprintf("hetzner: %s (%s)", e.Message, e.Code) }

func (e *hcError) Unwrap() error {
	switch e.Code {
	case "resource_unavailable", "placement_error", "server_type_unavailable":
		return ErrUnavailable
	case "resource_limit_exceeded", "rate_limit_exceeded", "limit_exceeded":
		return ErrQuota
	}
	return nil
}

func (h *Hetzner) do(ctx context.Context, method, path string, body, out any) error {
	if h.Token == "" {
		return ErrNotConfigured
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := h.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("hetzner: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error hcError `json:"error"`
		}
		json.Unmarshal(data, &e)
		e.Error.Status = res.StatusCode
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(data))
			if e.Error.Message == "" {
				e.Error.Message = res.Status
			}
		}
		return &e.Error
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func labelSelector(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return url.QueryEscape(strings.Join(parts, ","))
}

func (h *Hetzner) Check(ctx context.Context) error {
	return h.do(ctx, "GET", "/locations", nil, nil)
}

// ── shared resources ────────────────────────────────────────────────────────

type hcFirewallRule struct {
	Direction string   `json:"direction"`
	Protocol  string   `json:"protocol"`
	Port      string   `json:"port,omitempty"`
	SourceIPs []string `json:"source_ips"`
}

func (h *Hetzner) Prepare(ctx context.Context, p PrepareRequest) error {
	if p.SSHAllowCIDR == "" {
		return fmt.Errorf("the IP address SSH may come from is not set")
	}
	rules := []hcFirewallRule{{Direction: "in", Protocol: "tcp", Port: "22", SourceIPs: []string{p.SSHAllowCIDR}}}

	var fws struct {
		Firewalls []struct {
			ID    int64            `json:"id"`
			Rules []hcFirewallRule `json:"rules"`
		} `json:"firewalls"`
	}
	if err := h.do(ctx, "GET", "/firewalls?name="+url.QueryEscape(firewallName(p.HubID)), nil, &fws); err != nil {
		return err
	}
	if len(fws.Firewalls) == 0 {
		err := h.do(ctx, "POST", "/firewalls", map[string]any{
			"name": firewallName(p.HubID), "rules": rules, "labels": map[string]string{LabelManaged: "true"},
		}, nil)
		if err != nil {
			return fmt.Errorf("create firewall: %w", err)
		}
	} else if fw := fws.Firewalls[0]; len(fw.Rules) != 1 || len(fw.Rules[0].SourceIPs) != 1 || fw.Rules[0].SourceIPs[0] != p.SSHAllowCIDR {
		if err := h.do(ctx, "POST", fmt.Sprintf("/firewalls/%d/actions/set_rules", fw.ID), map[string]any{"rules": rules}, nil); err != nil {
			return fmt.Errorf("update firewall: %w", err)
		}
	}

	return h.EnsureSSHKey(ctx, "chiperka-hub-"+p.HubID, p.HubPublicKey)
}

// EnsureSSHKey registers (or replaces) a named SSH key. Every server must get
// one, otherwise Hetzner e-mails a root password.
func (h *Hetzner) EnsureSSHKey(ctx context.Context, name, publicKey string) error {
	var keys struct {
		SSHKeys []struct {
			ID        int64  `json:"id"`
			PublicKey string `json:"public_key"`
		} `json:"ssh_keys"`
	}
	if err := h.do(ctx, "GET", "/ssh_keys?name="+url.QueryEscape(name), nil, &keys); err != nil {
		return err
	}
	if len(keys.SSHKeys) > 0 && sameKey(keys.SSHKeys[0].PublicKey, publicKey) {
		return nil
	}
	if len(keys.SSHKeys) > 0 {
		h.do(ctx, "DELETE", fmt.Sprintf("/ssh_keys/%d", keys.SSHKeys[0].ID), nil, nil)
	}
	return h.do(ctx, "POST", "/ssh_keys", map[string]any{
		"name": name, "public_key": publicKey, "labels": map[string]string{LabelManaged: "true"},
	}, nil)
}

// DeleteSSHKey removes a named SSH key if it exists.
func (h *Hetzner) DeleteSSHKey(ctx context.Context, name string) error {
	var keys struct {
		SSHKeys []struct {
			ID int64 `json:"id"`
		} `json:"ssh_keys"`
	}
	if err := h.do(ctx, "GET", "/ssh_keys?name="+url.QueryEscape(name), nil, &keys); err != nil {
		return err
	}
	for _, k := range keys.SSHKeys {
		h.do(ctx, "DELETE", fmt.Sprintf("/ssh_keys/%d", k.ID), nil, nil)
	}
	return nil
}

func sameKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

// ── servers ─────────────────────────────────────────────────────────────────

type hcServer struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Created    string `json:"created"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	Datacenter struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
	Labels map[string]string `json:"labels"`
}

func (s *hcServer) instance() Instance {
	in := Instance{ID: strconv.FormatInt(s.ID, 10), Name: s.Name, Type: s.ServerType.Name, Location: s.Datacenter.Location.Name, Labels: s.Labels}
	if s.PublicNet.IPv4 != nil {
		in.PublicIP = s.PublicNet.IPv4.IP
	}
	in.CreatedAt, _ = time.Parse(time.RFC3339, s.Created)
	switch s.Status {
	case "running":
		in.State = StateRunning
	case "off", "stopping":
		in.State = StateStopped
	case "deleting":
		in.State = StateTerminated
	default: // initializing, starting, rebuilding, migrating
		in.State = StatePending
	}
	return in
}

func (h *Hetzner) Create(ctx context.Context, r CreateRequest) (*Instance, error) {
	image := r.Image
	if image == "" {
		if img, err := h.LatestImage(ctx); err == nil && img != nil {
			image = img.ID
		} else if image, err = h.BaseImage(ctx); err != nil {
			return nil, err
		}
	}
	var fwID int64
	var fws struct {
		Firewalls []struct {
			ID int64 `json:"id"`
		} `json:"firewalls"`
	}
	// Never create a server without the firewall: SSH would be open to the world.
	if err := h.do(ctx, "GET", "/firewalls?name="+url.QueryEscape(firewallName(r.Labels[LabelHub])), nil, &fws); err != nil {
		return nil, err
	}
	if len(fws.Firewalls) == 0 {
		return nil, fmt.Errorf("hetzner: firewall %s is missing, run Prepare first", firewallName(r.Labels[LabelHub]))
	}
	fwID = fws.Firewalls[0].ID
	var sshKeys []string
	if hub := r.Labels[LabelHub]; hub != "" {
		sshKeys = []string{"chiperka-hub-" + hub}
	}
	body := map[string]any{
		"name":        r.Name,
		"server_type": r.Type,
		"location":    r.Location,
		"image":       hcImageRef(image),
		"user_data":   r.UserData,
		"labels":      r.Labels,
		"ssh_keys":    sshKeys,
		"public_net":  map[string]bool{"enable_ipv4": true, "enable_ipv6": true},
	}
	body["firewalls"] = []map[string]int64{{"firewall": fwID}}
	var out struct {
		Server hcServer `json:"server"`
	}
	if err := h.do(ctx, "POST", "/servers", body, &out); err != nil {
		return nil, err
	}
	in := out.Server.instance()
	return &in, nil
}

// hcImageRef: the API takes an image ID or name as a string.
func hcImageRef(image string) any { return image }

func (h *Hetzner) Delete(ctx context.Context, id string) error {
	err := h.do(ctx, "DELETE", "/servers/"+id, nil, nil)
	if e, ok := err.(*hcError); ok && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (h *Hetzner) List(ctx context.Context) ([]Instance, error) {
	var out []Instance
	for page := 1; ; page++ {
		var res struct {
			Servers []hcServer `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		path := fmt.Sprintf("/servers?per_page=50&page=%d&label_selector=%s", page, labelSelector(map[string]string{LabelManaged: "true"}))
		if err := h.do(ctx, "GET", path, nil, &res); err != nil {
			return nil, err
		}
		for i := range res.Servers {
			out = append(out, res.Servers[i].instance())
		}
		if res.Meta.Pagination.NextPage == nil {
			return out, nil
		}
	}
}

// ── prices ──────────────────────────────────────────────────────────────────

type hcServerType struct {
	Name   string `json:"name"`
	Prices []struct {
		Location    string `json:"location"`
		PriceHourly struct {
			Net string `json:"net"`
		} `json:"price_hourly"`
	} `json:"prices"`
}

func (h *Hetzner) serverType(ctx context.Context, name string) (*hcServerType, error) {
	var res struct {
		ServerTypes []hcServerType `json:"server_types"`
	}
	if err := h.do(ctx, "GET", "/server_types?name="+url.QueryEscape(name), nil, &res); err != nil {
		return nil, err
	}
	if len(res.ServerTypes) == 0 {
		return nil, fmt.Errorf("hetzner has no server type %q", name)
	}
	return &res.ServerTypes[0], nil
}

func (h *Hetzner) HourlyPrice(ctx context.Context, serverType, location string, _ bool) (float64, error) {
	st, err := h.serverType(ctx, serverType)
	if err != nil {
		return 0, err
	}
	for _, p := range st.Prices {
		if p.Location == location || location == "" {
			return strconv.ParseFloat(p.PriceHourly.Net, 64)
		}
	}
	return 0, fmt.Errorf("hetzner: %s is not offered in %s", serverType, location)
}

func (h *Hetzner) ValidateType(ctx context.Context, serverType string) error {
	_, err := h.serverType(ctx, serverType)
	return err
}

// ── images ──────────────────────────────────────────────────────────────────

func (h *Hetzner) BaseImage(context.Context) (string, error) { return "ubuntu-24.04", nil }

type hcImage struct {
	ID          int64   `json:"id"`
	Description string  `json:"description"`
	Created     string  `json:"created"`
	ImageSize   float64 `json:"image_size"`
	Status      string  `json:"status"`
}

func (i hcImage) image() Image {
	t, _ := time.Parse(time.RFC3339, i.Created)
	return Image{ID: strconv.FormatInt(i.ID, 10), Name: i.Description, CreatedAt: t, SizeGB: i.ImageSize}
}

func (h *Hetzner) ListImages(ctx context.Context) ([]Image, error) {
	var res struct {
		Images []hcImage `json:"images"`
	}
	path := "/images?type=snapshot&sort=created:desc&per_page=50&label_selector=" + labelSelector(map[string]string{LabelImage: ImageKind})
	if err := h.do(ctx, "GET", path, nil, &res); err != nil {
		return nil, err
	}
	out := []Image{}
	for _, i := range res.Images {
		if i.Status == "available" {
			out = append(out, i.image())
		}
	}
	return out, nil
}

func (h *Hetzner) LatestImage(ctx context.Context) (*Image, error) {
	imgs, err := h.ListImages(ctx)
	if err != nil || len(imgs) == 0 {
		return nil, err
	}
	return &imgs[0], nil
}

func (h *Hetzner) CreateImage(ctx context.Context, instanceID, name string) (*Image, error) {
	var res struct {
		Image  hcImage `json:"image"`
		Action struct {
			ID int64 `json:"id"`
		} `json:"action"`
	}
	err := h.do(ctx, "POST", "/servers/"+instanceID+"/actions/create_image", map[string]any{
		"type": "snapshot", "description": name, "labels": map[string]string{LabelManaged: "true", LabelImage: ImageKind},
	}, &res)
	if err != nil {
		return nil, err
	}
	if err := h.waitAction(ctx, res.Action.ID, 60*time.Minute); err != nil {
		return nil, err
	}
	img := res.Image.image()
	img.Name = name
	return &img, nil
}

func (h *Hetzner) DeleteImage(ctx context.Context, id string) error {
	return h.do(ctx, "DELETE", "/images/"+id, nil, nil)
}

func (h *Hetzner) waitAction(ctx context.Context, id int64, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var res struct {
			Action struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"action"`
		}
		if err := h.do(ctx, "GET", fmt.Sprintf("/actions/%d", id), nil, &res); err != nil {
			return err
		}
		switch res.Action.Status {
		case "success":
			return nil
		case "error":
			if res.Action.Error != nil {
				return fmt.Errorf("hetzner action failed: %s", res.Action.Error.Message)
			}
			return fmt.Errorf("hetzner action failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

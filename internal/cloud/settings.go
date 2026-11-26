package cloud

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Rule: when at least MinQueue tests wait, keep Servers servers.
type Rule struct {
	MinQueue int `json:"min_queue"`
	Servers  int `json:"servers"`
}

// WarmWindow keeps Servers servers running during a time window, so the first
// run of the day does not wait for servers to start. Days: 1 = Monday … 7 = Sunday.
type WarmWindow struct {
	Days    []int  `json:"days"`
	From    string `json:"from"` // "08:00", hub local time
	To      string `json:"to"`   // "17:00"
	Servers int    `json:"servers"`
}

// ProviderSettings are the settings that differ per provider.
type ProviderSettings struct {
	Rules      []Rule   `json:"rules"`
	MaxServers int      `json:"max_servers"`
	Types      []string `json:"types"`     // in order of preference
	Locations  []string `json:"locations"` // Hetzner locations / AWS availability zones, in order of preference
}

// Settings drive the autoscaler. They live in the database and are edited in the
// UI at runtime. Provider credentials are not here: they come from the environment.
type Settings struct {
	Enabled       bool    `json:"enabled"`
	Provider      string  `json:"provider"`       // "hetzner" or "aws"
	MonthlyBudget float64 `json:"monthly_budget"` // in the provider's currency; 0 = no limit

	ScaleUpDelaySec int          `json:"scale_up_delay_sec"`
	WarmPool        []WarmWindow `json:"warm_pool"`

	IdleTimeoutSec        int  `json:"idle_timeout_sec"`
	HetznerHoldHour       bool `json:"hetzner_hold_hour"`
	HetznerHourReserveSec int  `json:"hetzner_hour_reserve_sec"`
	MaxLifetimeSec        int  `json:"max_lifetime_sec"`
	DrainTimeoutSec       int  `json:"drain_timeout_sec"`
	StartTimeoutSec       int  `json:"start_timeout_sec"`

	Hetzner           ProviderSettings `json:"hetzner"`
	AWS               ProviderSettings `json:"aws"`
	AWSSpot           bool             `json:"aws_spot"`
	AWSSpotToOnDemand bool             `json:"aws_spot_to_on_demand"`
	DiskGB            int              `json:"disk_gb"`

	WorkerConcurrency int    `json:"worker_concurrency"`
	WorkerImage       string `json:"worker_image"` // empty = the hub's CHIPERKA_WORKER_IMAGE
	ServerImage       string `json:"server_image"` // provider image ID; empty = newest prepared image

	SSHAllowCIDR string `json:"ssh_allow_cidr"` // public IP of the hub, e.g. "203.0.113.7/32"
}

// DefaultSettings are the values from the specification. Phase 0 measurements tune them.
func DefaultSettings() Settings {
	return Settings{
		Enabled:               true,
		Provider:              "hetzner",
		MonthlyBudget:         50,
		ScaleUpDelaySec:       15,
		WarmPool:              []WarmWindow{},
		IdleTimeoutSec:        180,
		HetznerHoldHour:       true,
		HetznerHourReserveSec: 300,
		MaxLifetimeSec:        4 * 3600,
		DrainTimeoutSec:       600,
		StartTimeoutSec:       300,
		Hetzner: ProviderSettings{
			Rules:      []Rule{{MinQueue: 40, Servers: 2}, {MinQueue: 300, Servers: 4}},
			MaxServers: 6,
			Types:      []string{"cpx62", "ccx33"},
			Locations:  []string{"nbg1", "fsn1"},
		},
		AWS: ProviderSettings{
			Rules:      []Rule{{MinQueue: 40, Servers: 4}, {MinQueue: 300, Servers: 8}},
			MaxServers: 8,
			Types:      []string{"c7a.4xlarge", "c6a.4xlarge", "m7a.4xlarge"},
			Locations:  []string{"eu-central-1a", "eu-central-1b", "eu-central-1c"},
		},
		AWSSpot:           true,
		AWSSpotToOnDemand: true,
		DiskGB:            60,
		WorkerConcurrency: 8,
	}
}

// Presets fill several settings at once; the user can fine-tune afterwards.
var Presets = map[string]func(*Settings){
	"economical": func(s *Settings) {
		s.Hetzner.Rules = []Rule{{MinQueue: 60, Servers: 1}, {MinQueue: 300, Servers: 2}}
		s.Hetzner.MaxServers = 3
		s.AWS.Rules = []Rule{{MinQueue: 60, Servers: 2}, {MinQueue: 300, Servers: 4}}
		s.AWS.MaxServers = 4
		s.IdleTimeoutSec = 120
	},
	"balanced": func(s *Settings) {
		d := DefaultSettings()
		s.Hetzner.Rules, s.Hetzner.MaxServers = d.Hetzner.Rules, d.Hetzner.MaxServers
		s.AWS.Rules, s.AWS.MaxServers = d.AWS.Rules, d.AWS.MaxServers
		s.IdleTimeoutSec = d.IdleTimeoutSec
	},
	"fast": func(s *Settings) {
		s.Hetzner.Rules = []Rule{{MinQueue: 20, Servers: 3}, {MinQueue: 300, Servers: 6}}
		s.Hetzner.MaxServers = 8
		s.AWS.Rules = []Rule{{MinQueue: 20, Servers: 6}, {MinQueue: 300, Servers: 12}}
		s.AWS.MaxServers = 12
		s.IdleTimeoutSec = 300
	},
}

// Clone returns a deep copy (the settings hold slices).
func (s Settings) Clone() Settings {
	raw, _ := json.Marshal(s)
	var c Settings
	json.Unmarshal(raw, &c)
	return c
}

// Active returns the settings of the selected provider.
func (s *Settings) Active() *ProviderSettings {
	if s.Provider == "aws" {
		return &s.AWS
	}
	return &s.Hetzner
}

// TargetFor returns how many servers the rules want for a queue of n tests.
func (p *ProviderSettings) TargetFor(n int) int {
	want := 0
	for _, r := range p.Rules {
		if n >= r.MinQueue && r.Servers > want {
			want = r.Servers
		}
	}
	return want
}

// WarmTarget returns the servers the warm pool wants at t.
func (s *Settings) WarmTarget(t time.Time) int {
	want := 0
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	hm := t.Format("15:04")
	for _, w := range s.WarmPool {
		day := false
		for _, d := range w.Days {
			day = day || d == wd
		}
		if day && hm >= w.From && hm < w.To && w.Servers > want {
			want = w.Servers
		}
	}
	return want
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// Validate rejects nonsense; the messages are shown in the UI next to the form.
func (s *Settings) Validate() []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if s.Provider != "hetzner" && s.Provider != "aws" {
		add("provider must be hetzner or aws")
	}
	if s.MonthlyBudget < 0 {
		add("monthly budget cannot be negative")
	}
	for name, v := range map[string]int{
		"idle time before shutdown": s.IdleTimeoutSec,
		"maximum server lifetime":   s.MaxLifetimeSec, "wait for running tests": s.DrainTimeoutSec,
		"wait for start": s.StartTimeoutSec, "disk size": s.DiskGB, "parallel tests per server": s.WorkerConcurrency,
	} {
		if v <= 0 {
			add("%s must be positive", name)
		}
	}
	if s.ScaleUpDelaySec < 0 {
		add("scale-up delay cannot be negative")
	}
	if s.MaxLifetimeSec+s.DrainTimeoutSec > int(SelfDestructAfter.Seconds())-600 {
		add("maximum lifetime plus the wait for running tests must stay under %s (servers power themselves off then)", roundDur(SelfDestructAfter-10*time.Minute))
	}
	if s.HetznerHourReserveSec < 0 || s.HetznerHourReserveSec >= 3600 {
		add("reserve before the end of the hour must be between 0 and 59 minutes")
	}
	for name, p := range map[string]*ProviderSettings{"Hetzner": &s.Hetzner, "AWS": &s.AWS} {
		last, highest := Rule{MinQueue: -1, Servers: -1}, 0
		for i, r := range p.Rules {
			highest = max(highest, r.Servers)
			if r.MinQueue <= last.MinQueue {
				add("%s: queue thresholds must increase (row %d)", name, i+1)
			}
			if r.Servers < last.Servers {
				add("%s: server counts must not decrease with a longer queue (row %d)", name, i+1)
			}
			if r.MinQueue < 0 || r.Servers < 0 {
				add("%s: rule values cannot be negative (row %d)", name, i+1)
			}
			last = r
		}
		if p.MaxServers < 0 {
			add("%s: maximum servers cannot be negative", name)
		}
		if p.MaxServers < highest {
			add("%s: maximum servers (%d) is lower than the highest rule (%d)", name, p.MaxServers, highest)
		}
		if len(p.Types) == 0 {
			add("%s: at least one server type is required", name)
		}
		if len(p.Locations) == 0 {
			add("%s: at least one location is required", name)
		}
	}
	for i, w := range s.WarmPool {
		if _, err := time.Parse("15:04", w.From); err != nil {
			add("warm pool row %d: invalid start time", i+1)
		}
		if _, err := time.Parse("15:04", w.To); err != nil || w.To <= w.From {
			add("warm pool row %d: end must be after start", i+1)
		}
		if w.Servers < 0 || len(w.Days) == 0 {
			add("warm pool row %d: pick days and a server count", i+1)
		}
	}
	if s.SSHAllowCIDR != "" {
		if _, _, err := net.ParseCIDR(s.SSHAllowCIDR); err != nil {
			add("SSH source must be an IP range like 203.0.113.7/32")
		}
	}
	sort.Strings(errs)
	return errs
}

// Normalize fills derived and defaulted values after decoding user input.
func (s *Settings) Normalize() {
	if ip := net.ParseIP(strings.TrimSpace(s.SSHAllowCIDR)); ip != nil {
		if ip.To4() != nil {
			s.SSHAllowCIDR = ip.String() + "/32"
		} else {
			s.SSHAllowCIDR = ip.String() + "/128"
		}
	}
	for _, p := range []*ProviderSettings{&s.Hetzner, &s.AWS} {
		p.Types = trimList(p.Types)
		p.Locations = trimList(p.Locations)
		if p.Rules == nil {
			p.Rules = []Rule{}
		}
	}
	if s.WarmPool == nil {
		s.WarmPool = []WarmWindow{}
	}
}

func trimList(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

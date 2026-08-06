package deadman

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

type TLSConfig struct {
	CAFile         string `yaml:"ca_file"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
	ServerName     string `yaml:"server_name"`
}

type FreshnessConfig struct {
	URL    string   `yaml:"url"`
	MaxAge Duration `yaml:"max_age"`
	// prometheus_scalar_age expects the Prometheus scalar response produced by
	// a query such as time()-timestamp(up{job="heimdall"}).
	Mode string `yaml:"mode"`
}

type TargetConfig struct {
	ID              string           `yaml:"id"`
	Kind            string           `yaml:"kind"`
	URL             string           `yaml:"url"`
	BearerTokenFile string           `yaml:"bearer_token_file"`
	TLS             TLSConfig        `yaml:"tls"`
	Freshness       *FreshnessConfig `yaml:"freshness,omitempty"`
	// AnchorURL, only meaningful on a "heimdall" target, is the audit-anchor
	// pull endpoint (ADR 0015 default sink). Empty means this target does not
	// participate in anchoring. It shares BearerTokenFile and TLS with the
	// rest of the target — the operator points that credential at Heimdall's
	// audit.anchor.credential_file, not at whatever authorizes /health/live.
	AnchorURL string `yaml:"anchor_url,omitempty"`
}

type NotificationConfig struct {
	URL             string    `yaml:"url"`
	BearerTokenFile string    `yaml:"bearer_token_file"`
	TLS             TLSConfig `yaml:"tls"`
	Timeout         Duration  `yaml:"timeout"`
	RetryMin        Duration  `yaml:"retry_min"`
	RetryMax        Duration  `yaml:"retry_max"`
}

type Config struct {
	ProbeID        string   `yaml:"probe_id"`
	Environment    string   `yaml:"environment"`
	Region         string   `yaml:"region"`
	Cluster        string   `yaml:"cluster"`
	Interval       Duration `yaml:"interval"`
	Timeout        Duration `yaml:"timeout"`
	HeartbeatTTL   Duration `yaml:"heartbeat_ttl"`
	FailuresToDown int      `yaml:"failures_to_down"`
	SuccessesToUp  int      `yaml:"successes_to_up"`
	OutboxLimit    int      `yaml:"outbox_limit"`
	DeliveryBatch  int      `yaml:"delivery_batch"`
	StateFile      string   `yaml:"state_file"`
	AuditFile      string   `yaml:"audit_file"`
	// AnchorFile is where pulled audit anchors (ADR 0015) are appended,
	// JSON-lines, one per line — the file `heimdall audit verify-anchor
	// --anchors` reads. Empty disables anchor pulling even if a target sets
	// AnchorURL, the same way AuditFile is required unconditionally today.
	AnchorFile   string             `yaml:"anchor_file,omitempty"`
	Notification NotificationConfig `yaml:"notification"`
	Targets      []TargetConfig     `yaml:"targets"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode dead-man config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var problems []error
	for name, value := range map[string]string{"probe_id": c.ProbeID, "environment": c.Environment, "region": c.Region, "cluster": c.Cluster, "state_file": c.StateFile, "audit_file": c.AuditFile} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}
	if c.Interval.Value() < 10*time.Second {
		problems = append(problems, errors.New("interval must be at least 10s"))
	}
	if c.Timeout.Value() <= 0 || c.Timeout.Value() >= c.Interval.Value() {
		problems = append(problems, errors.New("timeout must be positive and shorter than interval"))
	}
	if c.HeartbeatTTL.Value() < 2*c.Interval.Value() {
		problems = append(problems, errors.New("heartbeat_ttl must be at least twice interval"))
	}
	if c.FailuresToDown < 1 || c.SuccessesToUp < 1 {
		problems = append(problems, errors.New("transition thresholds must be positive"))
	}
	if c.OutboxLimit < 16 || c.OutboxLimit > 65536 || c.DeliveryBatch < 1 || c.DeliveryBatch > c.OutboxLimit {
		problems = append(problems, errors.New("outbox_limit must be 16..65536 and delivery_batch must be within it"))
	}
	if err := validateEndpoint("notification", c.Notification.URL, c.Notification.BearerTokenFile, c.Notification.TLS); err != nil {
		problems = append(problems, err)
	}
	if c.Notification.Timeout.Value() <= 0 || c.Notification.RetryMin.Value() <= 0 || c.Notification.RetryMax.Value() < c.Notification.RetryMin.Value() {
		problems = append(problems, errors.New("notification timeout and retry bounds are invalid"))
	}
	wanted := map[string]bool{"heimdall": false, "prometheus": false, "alertmanager": false}
	seen := make(map[string]bool)
	for _, target := range c.Targets {
		if target.ID == "" || seen[target.ID] {
			problems = append(problems, fmt.Errorf("target id %q is empty or duplicated", target.ID))
		}
		seen[target.ID] = true
		if _, ok := wanted[target.Kind]; !ok {
			problems = append(problems, fmt.Errorf("target %q has unsupported kind %q", target.ID, target.Kind))
		} else {
			wanted[target.Kind] = true
		}
		if err := validateEndpoint("target "+target.ID, target.URL, target.BearerTokenFile, target.TLS); err != nil {
			problems = append(problems, err)
		}
		if target.Freshness != nil {
			if target.Freshness.Mode != "prometheus_scalar_age" || target.Freshness.MaxAge.Value() <= 0 {
				problems = append(problems, fmt.Errorf("target %q freshness configuration is invalid", target.ID))
			}
			if err := validateHTTPS(target.Freshness.URL); err != nil {
				problems = append(problems, fmt.Errorf("target %q freshness URL: %w", target.ID, err))
			}
		}
		if target.Kind == "prometheus" && target.Freshness == nil {
			problems = append(problems, fmt.Errorf("target %q must configure a freshness query", target.ID))
		}
		if target.AnchorURL != "" {
			if target.Kind != "heimdall" {
				problems = append(problems, fmt.Errorf("target %q sets anchor_url but is not a heimdall target", target.ID))
			}
			if c.AnchorFile == "" {
				problems = append(problems, fmt.Errorf("target %q sets anchor_url but anchor_file is not configured", target.ID))
			}
			if err := validateHTTPS(target.AnchorURL); err != nil {
				problems = append(problems, fmt.Errorf("target %q anchor URL: %w", target.ID, err))
			}
		}
	}
	for kind, present := range wanted {
		if !present {
			problems = append(problems, fmt.Errorf("one %s target is required", kind))
		}
	}
	return errors.Join(problems...)
}

func validateEndpoint(name, rawURL, tokenFile string, tlsConfig TLSConfig) error {
	if err := validateHTTPS(rawURL); err != nil {
		return fmt.Errorf("%s URL: %w", name, err)
	}
	if tlsConfig.CAFile == "" {
		return fmt.Errorf("%s tls.ca_file is required", name)
	}
	if (tlsConfig.ClientCertFile == "") != (tlsConfig.ClientKeyFile == "") {
		return fmt.Errorf("%s client certificate and key must be configured together", name)
	}
	if tokenFile == "" && tlsConfig.ClientCertFile == "" {
		return fmt.Errorf("%s requires bearer_token_file or mTLS", name)
	}
	return nil
}

func validateHTTPS(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("must be an absolute HTTPS URL without userinfo")
	}
	return nil
}

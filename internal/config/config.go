package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"gopkg.in/yaml.v3"
)

// SchemaVersion is the shape of the configuration file this binary writes and
// reads. It advances when a key is retired, which is what gives a file's
// `version` something to say: v0.3.0 through v0.8.5 all shipped version 1
// across two retirements, so the key recorded nothing and an older file could
// not be told apart from a current one. `halro config migrate` is what moves a
// file from one version to the next.
const SchemaVersion = 2

type Config struct {
	Version               int                   `yaml:"version"`
	Server                Server                `yaml:"server"`
	TLS                   TLS                   `yaml:"tls"`
	Storage               Storage               `yaml:"storage"`
	Admin                 Admin                 `yaml:"admin"`
	Usage                 Usage                 `yaml:"usage"`
	Ledger                Ledger                `yaml:"ledger"`
	Gateway               Gateway               `yaml:"gateway"`
	Retry                 Retry                 `yaml:"retry"`
	Routing               Routing               `yaml:"routing"`
	Alerts                Alerts                `yaml:"alerts"`
	Security              Security              `yaml:"security"`
	Metrics               Metrics               `yaml:"metrics"`
	Audit                 Audit                 `yaml:"audit"`
	ModelCatalog          ModelCatalog          `yaml:"model_catalog"`
	ProviderSubscriptions ProviderSubscriptions `yaml:"provider_subscriptions"`
	// LegacyProviders keeps v0.8.1 configuration files readable. Provider
	// connection defaults moved into the Admin-managed credential workflow in
	// v0.8.2, so this section is validated but no longer drives runtime state.
	LegacyProviders LegacyProviders `yaml:"providers,omitempty"`
	Logging         Logging         `yaml:"logging"`
}

// Logging configures the process log: what is written, in which encoding, and
// whether a copy lands on this host.
//
// It is deliberately small. Records are redacted on the way out no matter what
// is set here — that is a property of the logger, not a switch — and there is no
// per-package level, no sampling, and no network sink. What an operator needs
// from this file is a level, an encoding their tooling reads, and a bounded file
// they can grep after the terminal has scrolled away.
type Logging struct {
	// Level is the lowest severity written: debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is json for machine reading or text for a human at a terminal.
	Format string `yaml:"format"`
	// Output is stderr, file, or both. "both" is what a containerised deployment
	// wants when it also keeps a local file: the platform collects the stream,
	// the file survives a collector outage.
	Output string `yaml:"output"`
	// File is where the log is written when Output includes a file. Empty means
	// logs/halro.log inside the data directory, which is already this instance's
	// private, exclusively-locked directory.
	File string `yaml:"file"`
	// MaxSizeMB rotates the file once a record would carry it past this size.
	MaxSizeMB int `yaml:"max_size_mb"`
	// MaxFiles counts every generation kept, including the file being written.
	MaxFiles int `yaml:"max_files"`
	// ErrorFile is a second, ERROR-only copy of the log.
	ErrorFile ErrorFile `yaml:"error_file"`
}

// ErrorFile is the errors-only log, written beside the ordinary one rather than
// instead of it.
//
// Setting `level: error` gets an error-only main log and loses everything that
// made the ordinary one worth keeping: a certificate nearing expiry, a probe
// that failed, an attempt that was retried before the request succeeded. This
// is the other half of that trade — stderr stays at info or warn, and a bounded
// file collects the ERRORs alone, which is what an incident starts from and
// what a support ticket is built out of.
//
// Its own level is fixed at ERROR and its own encoding is fixed at JSON. Both
// are the point of the file: a threshold that could be lowered would make it a
// second copy of the main log, and a text encoding would make the one file that
// exists to be searched by machine the harder of the two to search.
type ErrorFile struct {
	Enabled bool `yaml:"enabled"`
	// File is where it is written. Empty means logs/halro-error.log inside the
	// data directory, beside the ordinary log.
	File string `yaml:"file"`
	// MaxSizeMB and MaxFiles bound it the way they bound the ordinary log. They
	// matter more here: this file is the one an operator reads after an
	// incident, and a generation limit spent on noise is history they wanted.
	//
	// Zero means "not set", not "zero", and resolves to the built-in default.
	// The ordinary log's limits have no such rule because every config file
	// ever written carries them; this block did not exist until now, so an
	// install upgrading into it has them absent. Refusing to start over an
	// absent limit for a file that is switched off would brick every existing
	// data directory — a fail-closed check is only worth having when the thing
	// it closes on is a real ambiguity.
	MaxSizeMB int `yaml:"max_size_mb"`
	MaxFiles  int `yaml:"max_files"`
}

// DefaultErrorFileMaxSizeMB and DefaultErrorFileMaxFiles are what an unset
// limit resolves to. Smaller and deeper than the ordinary log's: this file
// takes far fewer records, and the generations are what an operator reads back
// through after an incident.
const (
	DefaultErrorFileMaxSizeMB = 32
	DefaultErrorFileMaxFiles  = 10
)

func (e ErrorFile) SizeLimitMB() int {
	if e.MaxSizeMB == 0 {
		return DefaultErrorFileMaxSizeMB
	}
	return e.MaxSizeMB
}

func (e ErrorFile) FileLimit() int {
	if e.MaxFiles == 0 {
		return DefaultErrorFileMaxFiles
	}
	return e.MaxFiles
}

const (
	LogOutputStderr = "stderr"
	LogOutputFile   = "file"
	LogOutputBoth   = "both"

	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Level maps the configured name onto a slog level. It answers info for an
// unrecognized name; Validate is what refuses one, and a logger built before
// validation has run should still write something.
func (l Logging) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(l.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (l Logging) WritesStderr() bool {
	return l.Output != LogOutputFile
}

func (l Logging) WritesFile() bool {
	return l.Output == LogOutputFile || l.Output == LogOutputBoth
}

// LogFilePath resolves the file the log is written to. An operator who names one
// gets exactly that path; the default lives beside the data this instance
// already owns exclusively.
func (c Config) LogFilePath() string {
	if path := strings.TrimSpace(c.Logging.File); path != "" {
		return path
	}
	return filepath.Join(c.Storage.DataDir, "logs", "halro.log")
}

// ErrorLogFilePath resolves the errors-only file, on the same rule as the
// ordinary one.
func (c Config) ErrorLogFilePath() string {
	if path := strings.TrimSpace(c.Logging.ErrorFile.File); path != "" {
		return path
	}
	return filepath.Join(c.Storage.DataDir, "logs", "halro-error.log")
}

type Server struct {
	GatewayListen     string   `yaml:"gateway_listen"`
	AdminListen       string   `yaml:"admin_listen"`
	MetricsListen     string   `yaml:"metrics_listen"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadBodyTimeout   Duration `yaml:"read_body_timeout"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	MaxHeaderBytes    int      `yaml:"max_header_bytes"`
	MaxRequestBytes   int64    `yaml:"max_request_bytes"`
}

type TLS struct {
	Enabled bool `yaml:"enabled"`
	// Certificates is ordered, and the order carries meaning: the first entry
	// answers connections that arrive without SNI, which is what a health check
	// dialling the address rather than the name does. Every other entry is
	// reached only by the names its certificate declares.
	Certificates []TLSCertificate `yaml:"certificates"`
}

type TLSCertificate struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// MaxTLSCertificates bounds the list so a malformed file cannot ask the process
// to open an unbounded number of keypairs at startup. A gateway serving more
// than a handful of distinct names belongs behind a proxy that terminates TLS.
const MaxTLSCertificates = 16

type Storage struct {
	DataDir      string    `yaml:"data_dir"`
	MetadataFile string    `yaml:"metadata_file"`
	MasterKey    MasterKey `yaml:"master_key"`
}

const (
	MasterKeyModeFile     = "file"
	MasterKeyModeKeySlots = "key_slots"
)

type MasterKey struct {
	Mode            string          `yaml:"mode"`
	File            string          `yaml:"file,omitempty"`
	PrimarySlot     string          `yaml:"primary_slot,omitempty"`
	RecoverySlot    string          `yaml:"recovery_slot,omitempty"`
	StartupDeadline Duration        `yaml:"startup_deadline,omitempty"`
	CallTimeout     Duration        `yaml:"call_timeout,omitempty"`
	AllowedKMSKeys  []AllowedKMSKey `yaml:"allowed_kms_keys,omitempty"`
}

type AllowedKMSKey struct {
	Purpose   string `yaml:"purpose"`
	Provider  string `yaml:"provider"`
	Region    string `yaml:"region,omitempty"`
	Account   string `yaml:"account,omitempty"`
	KeyID     string `yaml:"key_id"`
	Endpoint  string `yaml:"endpoint,omitempty"`
	Algorithm string `yaml:"algorithm,omitempty"`
}

type Usage struct {
	Durability             string   `yaml:"durability"`
	Timezone               string   `yaml:"timezone"`
	WALQueueCapacity       int      `yaml:"wal_queue_capacity"`
	WALMaxBatch            int      `yaml:"wal_max_batch"`
	WALFlushInterval       Duration `yaml:"wal_flush_interval"`
	AnalyticsQueueCapacity int      `yaml:"analytics_queue_capacity"`
	CheckpointInterval     Duration `yaml:"checkpoint_interval"`
	ParquetInterval        Duration `yaml:"parquet_interval"`
	// RetentionDays bounds the Parquet archive — how long exported partitions
	// are kept. It is not what the console can page through; see
	// ConsoleWindowDays.
	RetentionDays int `yaml:"retention_days"`
	// ConsoleWindowDays bounds the in-memory aggregate the attempt log and the
	// failed-request list are served from.
	//
	// It is a separate setting from RetentionDays because the two answer
	// different questions — how far back the screen goes, and how long the
	// archive is kept — and binding them would make an operator who needs a
	// long archive pay for it in memory. An attempt costs about a kilobyte
	// resident and about the same again in the checkpoint that holds it, so
	// this setting is a standing memory cost in a way the archive's length is
	// not; see the operator guide's sizing table.
	ConsoleWindowDays int `yaml:"console_window_days"`
	// ExportFormat selects the container new Usage partitions are written in
	// (ADR 0017): "parquet" (default) or "ndjson". Existing partitions are
	// never rewritten — this only changes what gets written from here on.
	ExportFormat string `yaml:"export_format"`
}

// The console window's default and floor. Thirty days is long enough that an
// operator investigating last month's incident still finds it, and short enough
// that the checkpoint stays a fixed cost rather than a growing one.
const (
	DefaultConsoleWindowDays = 30
	MinConsoleWindowDays     = 7
)

const (
	UsageExportFormatParquet = "parquet"
	UsageExportFormatNDJSON  = "ndjson"
)

// Ledger configures the accounting write-ahead log itself.
//
// It is separate from Usage because Usage settings govern a derivative — an
// aggregate, an archive, a rollup, all rebuildable — and these govern the
// authority they are derived from.
type Ledger struct {
	Seal LedgerSeal `yaml:"seal"`
}

// LedgerSeal controls whether the WAL is allowed to stop being one file.
//
// Default off. Sealing is the only mechanism in this system that lets the
// accounting authority's bytes move, and turning it on should be a decision an
// operator makes about their disk, not something a default does to them.
type LedgerSeal struct {
	Enabled bool `yaml:"enabled"`
	// MaxActiveBytes is the size the active generation may reach before it is
	// rolled off. Bytes rather than days: what runs out is disk, and a day of
	// history is a different number of bytes on every install.
	MaxActiveBytes int64 `yaml:"max_active_bytes"`
	// Compress replaces a sealed generation's plain bytes with a gzip copy on a
	// later maintenance tick. Measured at 5.6x on a real ledger.wal, paid once
	// per generation, off the request path entirely.
	Compress bool `yaml:"compress"`
}

// Sealing bounds. The floor is not a style preference: a generation smaller
// than this rolls often enough that the manifest, not the frames, becomes the
// bulk of the directory, and every roll is a full fsync of a renamed file.
const (
	DefaultLedgerSealMaxActiveBytes = int64(8) << 30
	MinLedgerSealMaxActiveBytes     = int64(16) << 20
)

type Admin struct {
	SessionTTL         Duration  `yaml:"session_ttl"`
	IdleTimeout        Duration  `yaml:"idle_timeout"`
	LoginRPM           int       `yaml:"login_rpm"`
	ExternalOrigin     string    `yaml:"external_origin"`
	SetupTokenFile     string    `yaml:"setup_token_file"`
	SetupTokenTTL      *Duration `yaml:"setup_token_ttl"`
	MFAPolicy          string    `yaml:"mfa_policy"`
	DeveloperWorkbench string    `yaml:"developer_workbench"`
	// ReauthElevationWindow is how long one proven re-authentication keeps
	// letting the same admin session perform step-up-guarded actions without
	// proving itself again.
	//
	// Step-up exists so that a stolen session alone cannot delete a Route,
	// replace a Provider credential, or edit a protection down to nothing. That
	// remains true of the second such action in a row, so the guard is not
	// dropped — it is amortised over the sitting an operator spends
	// administering, where the alternative was a password and a TOTP code per
	// row of a table.
	//
	// A pointer for the reason Gateway.SourceRateLimit.RequestsPerMinute is one:
	// absent and "the operator wrote 0" are different answers. Absent takes the
	// default; an explicit 0 asks on every action, for an operator who means it.
	// Normalize resolves the absent case, so everything downstream reads a
	// value.
	//
	// The window is bound to the session, never to the account: a second
	// session, stolen or otherwise, inherits nothing. It also does not cover the
	// admin-account endpoints — changing a password, removing an authenticator,
	// disabling MFA — which keep asking every time, because those are how an
	// intruder would make a stolen session permanent.
	ReauthElevationWindow    *Duration                `yaml:"reauth_elevation_window"`
	ModelCapabilityDetection ModelCapabilityDetection `yaml:"model_capability_detection"`
}

const (
	AdminMFAPolicyOptional               = "optional"
	AdminMFAPolicyRequired               = "required"
	AdminMFAPolicyAdministratorsRequired = "administrators_required"
)

const (
	DefaultAdminSetupTokenTTL = 30 * time.Minute
	MaxAdminSetupTokenTTL     = 24 * time.Hour
)

// MFARequiredForRole resolves the instance policy for one Admin-console role.
// An invalid role is never a valid stored AdminUser, but treating it as subject
// to the role-scoped policy keeps a corrupted identity from weakening MFA.
func (a Admin) MFARequiredForRole(role string) bool {
	return a.MFAPolicy == AdminMFAPolicyRequired ||
		(a.MFAPolicy == AdminMFAPolicyAdministratorsRequired && role != domain.AdminRoleReadOnly)
}

type ModelCapabilityDetection struct {
	FreshTTL            Duration `yaml:"fresh_ttl"`
	Retention           Duration `yaml:"retention"`
	RefreshCooldown     Duration `yaml:"refresh_cooldown"`
	TotalTimeout        Duration `yaml:"total_timeout"`
	GlobalConcurrency   int      `yaml:"global_concurrency"`
	ProviderConcurrency int      `yaml:"provider_concurrency"`
	MaxProviderCalls    int      `yaml:"max_provider_calls"`
	CreateRPM           int      `yaml:"create_rpm"`
}

// LegacyProviders is the retired v0.8.1 providers section. It remains in the
// decoding contract because configuration uses KnownFields: deleting the Go
// field would turn a supported in-place upgrade into a startup failure before
// the operator had any chance to remove the obsolete YAML.
type LegacyProviders struct {
	Bedrock LegacyBedrockProvider `yaml:"bedrock"`
}

type LegacyBedrockProvider struct {
	Region string `yaml:"region"`
}

const maxLegacyBedrockRegionLength = 64

func validLegacyBedrockRegion(region string) bool {
	if region == "" || len(region) > maxLegacyBedrockRegionLength || region[0] == '-' || region[len(region)-1] == '-' {
		return false
	}
	previousHyphen := false
	for _, character := range region {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			previousHyphen = false
		case character == '-':
			if previousHyphen {
				return false
			}
			previousHyphen = true
		default:
			return false
		}
	}
	return true
}

// ProviderSubscriptions decides whether this instance offers the consumer
// subscription products whose upstreams reserve them for their own clients:
// Claude Pro/Max as Claude Code signs in to it, and a ChatGPT plan as Codex
// signs in to it.
//
// Both default to false, and that default is the point. Halro's shipped
// behaviour is the one that complies with those terms without the operator
// having to know they exist: the Offering is absent from Admin metadata and
// every write path refuses it, exactly as a withheld profile is treated. The
// difference from Withheld is where the fact lives — a withheld profile is
// waiting on evidence, which is a property of the build, while this is a
// property of one operator's own arrangement with an upstream, which only they
// can speak to.
//
// What enabling one means, stated plainly because an operator turning it on
// deserves to read it in the file they are editing rather than in an issue:
//
//   - Anthropic's Claude Code legal and compliance page reserves OAuth sign-in
//     for its own clients, does not permit routing requests through Free, Pro or
//     Max credentials on behalf of users, and does not permit a developer to
//     collect, store or intermediate a Claude.ai credential.
//   - OpenAI's ChatGPT Terms of Use say you may not make your account available
//     to anyone else.
//
// Halro holding such a credential and presenting it upstream is what both
// describe. Neither upstream enforces it on the paths Halro would use — both
// were measured, see docs/verification/ — so nothing outside this switch stops
// it, which is precisely why the switch is off unless someone sets it.
//
// The supported production answer stays what it was: an Anthropic Console API
// key, or an OpenAI platform API key, each a metered product with its own
// billing. This section exists for an instance whose only user is the operator
// whose subscription it is — development and debugging against your own
// account.
type ProviderSubscriptions struct {
	// AnthropicClaude offers anthropic.claude-subscription.
	AnthropicClaude bool `yaml:"anthropic_claude"`
	// A member for the Codex subscription belongs here and is deliberately
	// absent until the rows it would open exist: a switch with nothing behind it
	// is a knob that does nothing, which this configuration refuses to grow.
}

// ModelCatalog governs optional signed background catalog updates. The remote
// endpoint, host allowlist and signature trust roots are compiled into Halro.
type ModelCatalog struct {
	Enabled             bool     `yaml:"enabled"`
	RefreshInterval     Duration `yaml:"refresh_interval"`
	PinnedRevision      string   `yaml:"pinned_revision,omitempty"`
	MaxDownloadBytes    int64    `yaml:"max_download_bytes"`
	MaxDecodedBytes     int64    `yaml:"max_decoded_bytes"`
	MaxCompressionRatio int64    `yaml:"max_compression_ratio"`
	MaxEntries          int      `yaml:"max_entries"`
}

type Gateway struct {
	RouteTotalTimeout             Duration        `yaml:"route_total_timeout"`
	AttemptConnectTimeout         Duration        `yaml:"attempt_connect_timeout"`
	AttemptResponseHeaderTimeout  Duration        `yaml:"attempt_response_header_timeout"`
	DownstreamWriteTimeout        Duration        `yaml:"downstream_write_timeout"`
	StreamMaxDuration             Duration        `yaml:"stream_max_duration"`
	MaxTotalAttempts              int             `yaml:"max_total_attempts"`
	DeferredResponseWorkers       int             `yaml:"deferred_response_workers"`
	HealthProbeInterval           Duration        `yaml:"health_probe_interval"`
	PricingClockRollbackTolerance Duration        `yaml:"pricing_clock_rollback_tolerance"`
	PricingClockForwardTolerance  Duration        `yaml:"pricing_clock_forward_tolerance"`
	PricingUnknownPolicy          string          `yaml:"pricing_unknown_policy"`
	SourceRateLimit               SourceRateLimit `yaml:"source_rate_limit"`
	FailureCapture                FailureCapture  `yaml:"failure_capture"`
}

// FailureCapture keeps the request body Halro accepted, its normalized form,
// and the answer or transport error the upstream gave it, so a failure can be
// reproduced rather than guessed at.
//
// It is off by default, and turning it on is a decision about what this
// instance's data directory contains rather than a verbosity setting. Nothing
// else Halro stores is material a caller wrote: prompts, tool arguments and
// response bodies are kept out of every log, metric and audit record, and that
// rule is unchanged. This is a separate, narrower act — encrypted under the
// master key, bound to the request and project it belongs to, bounded in size
// and count, expiring on a clock, and readable only through an audited admin
// action.
//
// Only failures are captured. A successful call is never stored, which is what
// keeps this a small tail of traffic rather than a copy of it.
type FailureCapture struct {
	Enabled bool `yaml:"enabled"`
	// MaxBytes bounds each captured part. The Gateway request, normalized
	// request and response are bounded separately: a large answer must not cost
	// either request view that explains it.
	MaxBytes int `yaml:"max_bytes"`
	// MaxRecordsPerDay bounds the store against an upstream that is failing
	// everything. Past it capture stops for the day and says so once, rather
	// than competing with the ledger for the same disk.
	MaxRecordsPerDay int `yaml:"max_records_per_day"`
	// Retain is how long a capture lives. This is the answer to "how long do we
	// keep customer prompts", enforced by a sweep rather than promised in a
	// runbook, so it is bounded at both ends: long enough to be useful after a
	// weekend, short enough that it is not an archive.
	Retain Duration `yaml:"retain"`
}

// Defaults an omitted key takes. They are deliberately conservative: a capture
// large enough to hold a whole conversation, a day's worth bounded well below
// what the ledger writes, and a window measured in hours rather than months.
const (
	DefaultFailureCaptureMaxBytes         = 64 << 10
	DefaultFailureCaptureMaxRecordsPerDay = 1000
	DefaultFailureCaptureRetain           = 24 * time.Hour
)

func (f FailureCapture) ByteLimit() int {
	if f.MaxBytes == 0 {
		return DefaultFailureCaptureMaxBytes
	}
	return f.MaxBytes
}

func (f FailureCapture) DailyRecordLimit() int {
	if f.MaxRecordsPerDay == 0 {
		return DefaultFailureCaptureMaxRecordsPerDay
	}
	return f.MaxRecordsPerDay
}

func (f FailureCapture) RetentionWindow() time.Duration {
	if f.Retain == 0 {
		return DefaultFailureCaptureRetain
	}
	return f.Retain.Value()
}

// defaultSourceRequestsPerMinute is the budget an absent
// gateway.source_rate_limit.requests_per_minute takes.
const defaultSourceRequestsPerMinute = 600

// defaultReauthElevationWindow is how long one proven re-authentication keeps
// letting the same admin session perform step-up-guarded actions.
//
// Long enough to cover the run of changes an operator makes in one sitting,
// which is the flow the per-action prompt was interrupting, and short enough
// that a session stolen afterwards is back to proving itself. It does not
// extend on use: the window is measured from the re-authentication, so a long
// sitting asks again rather than staying open indefinitely.
const defaultReauthElevationWindow = 10 * time.Minute

// Pricing clock tolerances an omitted gateway.pricing_clock_* key takes. Zero is
// not a usable value for either — a zero forward tolerance makes every priced
// attempt fail closed — so absence is filled rather than validated, the same way
// gateway.source_rate_limit is. Kept in step with Default() and default.yaml by
// TestDefaultTemplateMatchesDefault.
const (
	// DefaultDeferredResponseWorkers is what the deferred tier ran with when the
	// count was a constant nobody could reach. MaxDeferredResponseWorkers bounds
	// it because each worker can hold one upstream call open for
	// route_total_timeout, so this is concurrent upstream work, not just threads.
	DefaultDeferredResponseWorkers = 4
	MaxDeferredResponseWorkers     = 256

	DefaultPricingClockRollbackTolerance = 2 * time.Second
	DefaultPricingClockForwardTolerance  = 30 * time.Second
)

// MinPricingClockRollbackTolerance is a floor, not a default: price selections
// on one deployment run concurrently, so they can reach their durable pin in the
// reverse of the order they captured pricing_selected_at, and the later one then
// reads a selection time behind the high-water mark. That backwards step is
// caused by this process, not by the clock. The tolerance must therefore stay
// above the span a selection can spend between capturing its time and committing
// its pin — batch delay plus fsync plus scheduling — or ordinary concurrency
// would quarantine a deployment for a wall-clock rollback that never happened.
// See ADR 0012, "Amendment 2026-08-07".
const MinPricingClockRollbackTolerance = time.Second

// SourceRateLimit bounds anonymous data-plane work per source address, ahead of
// the per-project limiter — which cannot apply until a request has been
// authenticated, and so cannot bound the cost of authenticating it.
type SourceRateLimit struct {
	// RequestsPerMinute is the per-source budget. A pointer so that "the key is
	// absent" and "the operator wrote 0" are different answers: absent takes the
	// default, because a security control that switches itself off for every
	// config file written before it existed protects nobody, and an explicit 0
	// disables the limiter for an operator who means it. Normalize resolves the
	// absent case, so everything downstream reads a value.
	RequestsPerMinute *int `yaml:"requests_per_minute"`
	// MaxTrackedSources caps distinct addresses remembered within one minute so
	// the limiter cannot itself be grown without bound. Addresses past the cap
	// share one budget.
	MaxTrackedSources int `yaml:"max_tracked_sources"`
}

type Retry struct {
	MaxAttemptsPerTarget int      `yaml:"max_attempts_per_target"`
	BaseDelay            Duration `yaml:"base_delay"`
	MaxDelay             Duration `yaml:"max_delay"`
	Jitter               bool     `yaml:"jitter"`
}

// Routing is when a route target stops being offered and when it is tried
// again.
//
// Only the availability figures are here. What to do about an upstream that
// stated its refusal — out of quota, subscription lapsed, key revoked — is not
// configurable: those windows follow from who said what, and inviting an
// operator to tune them before anyone has measured the distribution is asking
// the wrong person. The distribution is what
// halro_provider_failure_reason_total is collecting.
type Routing struct {
	// AvailabilityFailures is how many consecutive failures with no stated
	// reason take a target out. More than one, because a single 5xx is noise.
	AvailabilityFailures int `yaml:"availability_failures"`
	// SuspendFor is the first suspension, doubling to MaxSuspendFor each time a
	// probe fails again.
	SuspendFor    Duration `yaml:"suspend_for"`
	MaxSuspendFor Duration `yaml:"max_suspend_for"`
	// ProbeRequests is how many requests may test a suspended target once its
	// window is up.
	ProbeRequests int `yaml:"probe_requests"`
}

type Alerts struct {
	QueueCapacity int      `yaml:"queue_capacity"`
	Workers       int      `yaml:"workers"`
	Timeout       Duration `yaml:"timeout"`
	MaxAttempts   int      `yaml:"max_attempts"`
	BaseDelay     Duration `yaml:"base_delay"`
	MaxDelay      Duration `yaml:"max_delay"`
	DedupCooldown Duration `yaml:"dedup_cooldown"`
}

type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return errors.New("duration must be a scalar")
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

type Security struct {
	AllowPrivateProviderEndpoints bool     `yaml:"allow_private_provider_endpoints"`
	AllowPrivateWebhooks          bool     `yaml:"allow_private_webhooks"`
	TrustProxyHeaders             bool     `yaml:"trust_proxy_headers"`
	TrustedProxyCIDRs             []string `yaml:"trusted_proxy_cidrs"`
}

type Metrics struct {
	Enabled              bool       `yaml:"enabled"`
	RequireAuth          bool       `yaml:"require_auth"`
	CredentialFile       string     `yaml:"credential_file"`
	MaxConcurrentScrapes int        `yaml:"max_concurrent_scrapes"`
	WriteTimeout         Duration   `yaml:"write_timeout"`
	TLS                  MetricsTLS `yaml:"tls"`
}

type MetricsTLS struct {
	Enabled      bool   `yaml:"enabled"`
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// AuditAnchorSink identifies where anchors (ADR 0015) are sent. Only
// AuditAnchorSinkDeadManPull is implemented; the others are reserved names so
// a deployment's config does not need to change again when they land.
const (
	AuditAnchorSinkDeadManPull  = "dead_man_pull"
	AuditAnchorSinkSyslog       = "syslog"
	AuditAnchorSinkS3ObjectLock = "s3_object_lock"
)

type Audit struct {
	Anchor AuditAnchor `yaml:"anchor"`
}

type AuditAnchor struct {
	Enabled        bool     `yaml:"enabled"`
	Sink           string   `yaml:"sink"`
	Interval       Duration `yaml:"interval"`
	RecordDelta    int      `yaml:"record_delta"`
	CredentialFile string   `yaml:"credential_file"`
}

type LoadOptions struct {
	AllowInsecurePublicGateway bool
	// SkipListenerValidation is only for offline commands that never bind a
	// socket, such as init. Runtime entry points must leave it false.
	SkipListenerValidation bool
}

func Load(path string, opts LoadOptions) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	cfg, err := Decode(file)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Normalize(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(opts); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Decode(r io.Reader) (Config, error) {
	source, err := io.ReadAll(r)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	// Retired keys are looked for before the strict decode, because otherwise
	// the operator meets `field circuit_breaker not found in type config.Config`
	// — a sentence about a Go type, not about the key that replaced theirs.
	if err := refuseRetiredKeys(source); err != nil {
		return Config{}, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("decode config: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("decode trailing config: %w", err)
	}
	return cfg, nil
}

func (c *Config) Normalize() error {
	var err error
	// Existing configuration files predate server.shutdown_timeout. In that
	// case inherit the effective route budget rather than falling back to a
	// fixed duration that could be shorter than an operator's longest request.
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = c.Gateway.RouteTotalTimeout
	}
	if c.Gateway.SourceRateLimit.RequestsPerMinute == nil {
		budget := defaultSourceRequestsPerMinute
		c.Gateway.SourceRateLimit.RequestsPerMinute = &budget
	}
	// Absent means the default rather than zero, so a configuration written
	// before this key existed keeps working: the deferred tier had a fixed four
	// workers and no way to say otherwise.
	if c.Gateway.DeferredResponseWorkers == 0 {
		c.Gateway.DeferredResponseWorkers = DefaultDeferredResponseWorkers
	}
	if c.Gateway.PricingClockRollbackTolerance == 0 {
		c.Gateway.PricingClockRollbackTolerance = Duration(DefaultPricingClockRollbackTolerance)
	}
	if c.Gateway.PricingClockForwardTolerance == 0 {
		c.Gateway.PricingClockForwardTolerance = Duration(DefaultPricingClockForwardTolerance)
	}
	// Keep the v0.8.1 normalization contract even though the value is now
	// compatibility-only. Harmless surrounding whitespace must not turn an
	// existing valid file into an upgrade failure.
	c.LegacyProviders.Bedrock.Region = strings.TrimSpace(c.LegacyProviders.Bedrock.Region)
	if c.Gateway.SourceRateLimit.MaxTrackedSources == 0 {
		// Omitting the ceiling means "whatever is sane", not "track nothing".
		// Kept in step with sourcelimit.DefaultMaxTrackedSources by
		// TestSourceRateLimitCeilingMatchesLimiterDefault.
		c.Gateway.SourceRateLimit.MaxTrackedSources = 16384
	}
	for index := range c.Storage.MasterKey.AllowedKMSKeys {
		key := &c.Storage.MasterKey.AllowedKMSKeys[index]
		if key.Provider == "aws-kms" && key.Algorithm == "" {
			key.Algorithm = "SYMMETRIC_DEFAULT"
		}
		if strings.HasSuffix(key.Endpoint, "/") {
			key.Endpoint = strings.TrimSuffix(key.Endpoint, "/")
		}
	}
	c.Storage.DataDir, err = cleanAbsolutePath(c.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("storage.data_dir: %w", err)
	}
	if c.Storage.MasterKey.File != "" {
		c.Storage.MasterKey.File, err = cleanAbsolutePath(c.Storage.MasterKey.File)
		if err != nil {
			return fmt.Errorf("storage.master_key.file: %w", err)
		}
	}
	// An explicit empty list and an absent key describe the same thing. Folding
	// one onto the other keeps the first-run template comparable to Default().
	if len(c.TLS.Certificates) == 0 {
		c.TLS.Certificates = nil
	}
	for index := range c.TLS.Certificates {
		entry := &c.TLS.Certificates[index]
		if entry.CertFile != "" {
			entry.CertFile, err = cleanAbsolutePath(entry.CertFile)
			if err != nil {
				return fmt.Errorf("tls.certificates[%d].cert_file: %w", index, err)
			}
		}
		if entry.KeyFile != "" {
			entry.KeyFile, err = cleanAbsolutePath(entry.KeyFile)
			if err != nil {
				return fmt.Errorf("tls.certificates[%d].key_file: %w", index, err)
			}
		}
	}
	for name, value := range map[string]*string{
		"metrics.credential_file":    &c.Metrics.CredentialFile,
		"metrics.tls.cert_file":      &c.Metrics.TLS.CertFile,
		"metrics.tls.key_file":       &c.Metrics.TLS.KeyFile,
		"metrics.tls.client_ca_file": &c.Metrics.TLS.ClientCAFile,
	} {
		if *value == "" {
			continue
		}
		*value, err = cleanAbsolutePath(*value)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if c.Retry.MaxAttemptsPerTarget == 0 {
		c.Retry.MaxAttemptsPerTarget = 1
	}
	if c.Retry.BaseDelay == 0 {
		c.Retry.BaseDelay = Duration(100 * time.Millisecond)
	}
	if c.Retry.MaxDelay == 0 {
		c.Retry.MaxDelay = Duration(2 * time.Second)
	}
	if c.Metrics.MaxConcurrentScrapes == 0 {
		c.Metrics.MaxConcurrentScrapes = 2
	}
	// A config file written before logging existed has an empty block, and an
	// empty level would otherwise validate as "not a level" and refuse to start.
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = LogFormatJSON
	}
	if c.Logging.Output == "" {
		c.Logging.Output = LogOutputStderr
	}
	if c.Logging.MaxSizeMB == 0 {
		c.Logging.MaxSizeMB = 64
	}
	if c.Logging.MaxFiles == 0 {
		c.Logging.MaxFiles = 5
	}
	if c.Metrics.WriteTimeout == 0 {
		c.Metrics.WriteTimeout = Duration(5 * time.Second)
	}
	if c.Gateway.HealthProbeInterval == 0 {
		c.Gateway.HealthProbeInterval = Duration(30 * time.Second)
	}
	if c.Routing.AvailabilityFailures == 0 {
		c.Routing.AvailabilityFailures = 5
	}
	if c.Routing.SuspendFor == 0 {
		c.Routing.SuspendFor = Duration(30 * time.Second)
	}
	if c.Routing.MaxSuspendFor == 0 {
		c.Routing.MaxSuspendFor = Duration(5 * time.Minute)
	}
	if c.Routing.ProbeRequests == 0 {
		c.Routing.ProbeRequests = 1
	}
	if c.Alerts.QueueCapacity == 0 {
		c.Alerts.QueueCapacity = 1024
	}
	if c.Alerts.Workers == 0 {
		c.Alerts.Workers = 2
	}
	if c.Alerts.Timeout == 0 {
		c.Alerts.Timeout = Duration(5 * time.Second)
	}
	if c.Alerts.MaxAttempts == 0 {
		c.Alerts.MaxAttempts = 3
	}
	if c.Alerts.BaseDelay == 0 {
		c.Alerts.BaseDelay = Duration(250 * time.Millisecond)
	}
	if c.Alerts.MaxDelay == 0 {
		c.Alerts.MaxDelay = Duration(5 * time.Second)
	}
	if c.Alerts.DedupCooldown == 0 {
		c.Alerts.DedupCooldown = Duration(time.Minute)
	}
	if c.Usage.WALQueueCapacity == 0 {
		c.Usage.WALQueueCapacity = 4096
	}
	if c.Usage.AnalyticsQueueCapacity == 0 {
		c.Usage.AnalyticsQueueCapacity = 4096
	}
	if c.Usage.WALMaxBatch == 0 {
		c.Usage.WALMaxBatch = 128
	}
	if c.Usage.WALFlushInterval == 0 {
		c.Usage.WALFlushInterval = Duration(2 * time.Millisecond)
	}
	if c.Usage.CheckpointInterval == 0 {
		c.Usage.CheckpointInterval = Duration(time.Minute)
	}
	if c.Usage.ParquetInterval == 0 {
		c.Usage.ParquetInterval = Duration(time.Hour)
	}
	if c.Usage.RetentionDays == 0 {
		c.Usage.RetentionDays = 90
	}
	if c.Usage.ConsoleWindowDays == 0 {
		// Defaulted against the archive, not to a constant. A file that set a
		// short retention and never mentioned the console window was valid
		// before this setting existed, and filling in a flat thirty days would
		// make Validate refuse to start over a key the operator never wrote.
		c.Usage.ConsoleWindowDays = DefaultConsoleWindowDays
		if c.Usage.RetentionDays >= MinConsoleWindowDays && c.Usage.RetentionDays < DefaultConsoleWindowDays {
			c.Usage.ConsoleWindowDays = c.Usage.RetentionDays
		}
	}
	if c.Usage.ExportFormat == "" {
		c.Usage.ExportFormat = UsageExportFormatParquet
	}
	if c.Ledger.Seal.MaxActiveBytes == 0 {
		c.Ledger.Seal.MaxActiveBytes = DefaultLedgerSealMaxActiveBytes
	}
	if c.Admin.SessionTTL == 0 {
		c.Admin.SessionTTL = Duration(8 * time.Hour)
	}
	if c.Admin.IdleTimeout == 0 {
		c.Admin.IdleTimeout = Duration(30 * time.Minute)
	}
	if c.Admin.LoginRPM == 0 {
		c.Admin.LoginRPM = 5
	}
	if c.Admin.SetupTokenTTL == nil {
		value := Duration(DefaultAdminSetupTokenTTL)
		c.Admin.SetupTokenTTL = &value
	}
	if c.Admin.MFAPolicy == "" {
		c.Admin.MFAPolicy = "optional"
	}
	if c.Admin.DeveloperWorkbench == "" {
		c.Admin.DeveloperWorkbench = "enabled"
	}
	if c.Admin.ReauthElevationWindow == nil {
		window := Duration(defaultReauthElevationWindow)
		c.Admin.ReauthElevationWindow = &window
	}
	// A negative window is not "ask every time by accident": it is normalised to
	// zero, which is the value that does ask every time, rather than left to be
	// compared against and quietly behave as an expired grant.
	if *c.Admin.ReauthElevationWindow < 0 {
		zero := Duration(0)
		c.Admin.ReauthElevationWindow = &zero
	}
	defaultDetection := Default().Admin.ModelCapabilityDetection
	if c.Admin.ModelCapabilityDetection.FreshTTL == 0 {
		c.Admin.ModelCapabilityDetection.FreshTTL = defaultDetection.FreshTTL
	}
	if c.Admin.ModelCapabilityDetection.Retention == 0 {
		c.Admin.ModelCapabilityDetection.Retention = defaultDetection.Retention
	}
	if c.Admin.ModelCapabilityDetection.RefreshCooldown == 0 {
		c.Admin.ModelCapabilityDetection.RefreshCooldown = defaultDetection.RefreshCooldown
	}
	if c.Admin.ModelCapabilityDetection.TotalTimeout == 0 {
		c.Admin.ModelCapabilityDetection.TotalTimeout = defaultDetection.TotalTimeout
	}
	if c.Admin.ModelCapabilityDetection.GlobalConcurrency == 0 {
		c.Admin.ModelCapabilityDetection.GlobalConcurrency = defaultDetection.GlobalConcurrency
	}
	if c.Admin.ModelCapabilityDetection.ProviderConcurrency == 0 {
		c.Admin.ModelCapabilityDetection.ProviderConcurrency = defaultDetection.ProviderConcurrency
	}
	if c.Admin.ModelCapabilityDetection.MaxProviderCalls == 0 {
		c.Admin.ModelCapabilityDetection.MaxProviderCalls = defaultDetection.MaxProviderCalls
	}
	if c.Admin.ModelCapabilityDetection.CreateRPM == 0 {
		c.Admin.ModelCapabilityDetection.CreateRPM = defaultDetection.CreateRPM
	}
	defaultCatalog := Default().ModelCatalog
	if c.ModelCatalog.RefreshInterval == 0 {
		c.ModelCatalog.RefreshInterval = defaultCatalog.RefreshInterval
	}
	if c.ModelCatalog.MaxDownloadBytes == 0 {
		c.ModelCatalog.MaxDownloadBytes = defaultCatalog.MaxDownloadBytes
	}
	if c.ModelCatalog.MaxDecodedBytes == 0 {
		c.ModelCatalog.MaxDecodedBytes = defaultCatalog.MaxDecodedBytes
	}
	if c.ModelCatalog.MaxCompressionRatio == 0 {
		c.ModelCatalog.MaxCompressionRatio = defaultCatalog.MaxCompressionRatio
	}
	if c.ModelCatalog.MaxEntries == 0 {
		c.ModelCatalog.MaxEntries = defaultCatalog.MaxEntries
	}
	return nil
}

func (c Config) Validate(opts LoadOptions) error {
	var problems []error
	switch {
	case c.Version == 0:
		// Separated from the older-file case because `config migrate` cannot
		// help here: it refuses a file that declares no shape rather than
		// guessing at one, so sending the operator there would name a command
		// that turns them straight back.
		problems = append(problems, fmt.Errorf(
			"no `version` is declared, so there is no shape to read this file as: add `version: %d` "+
				"if it came from a release older than this one and then run "+
				"`halro config migrate --config <path>`, or `version: %d` if it is current",
			SchemaVersion-1, SchemaVersion))
	case c.Version < SchemaVersion:
		// Directional on purpose. An older file has a way forward and is told
		// it; a newer one does not, and guessing at a shape this binary has
		// never seen is the fail-open version of this check.
		problems = append(problems, fmt.Errorf(
			"version is %d and this Halro writes %d: run `halro config migrate --config <path>` "+
				"to see what moved, then again with --write", c.Version, SchemaVersion))
	case c.Version > SchemaVersion:
		problems = append(problems, fmt.Errorf(
			"version is %d and this Halro only knows %d, so the file was written by a newer "+
				"Halro; run that one, or start from a configuration this version wrote",
			c.Version, SchemaVersion))
	}
	if c.Storage.DataDir == "" {
		problems = append(problems, errors.New("storage.data_dir is required"))
	}
	problems = append(problems, validateMasterKey(c.Storage.MasterKey)...)
	if c.Storage.MetadataFile == "" || filepath.Base(c.Storage.MetadataFile) != c.Storage.MetadataFile {
		problems = append(problems, errors.New("storage.metadata_file must be a file name without path components"))
	}
	problems = append(problems, validateLogging(c.Logging)...)
	if c.ModelCatalog.RefreshInterval < Duration(5*time.Minute) || c.ModelCatalog.RefreshInterval > Duration(7*24*time.Hour) {
		problems = append(problems, errors.New("model_catalog.refresh_interval must be between 5 minutes and 7 days"))
	}
	if c.ModelCatalog.MaxDownloadBytes < 4096 || c.ModelCatalog.MaxDownloadBytes > 16<<20 {
		problems = append(problems, errors.New("model_catalog.max_download_bytes must be between 4096 and 16777216"))
	}
	if c.ModelCatalog.MaxDecodedBytes < c.ModelCatalog.MaxDownloadBytes || c.ModelCatalog.MaxDecodedBytes > 64<<20 {
		problems = append(problems, errors.New("model_catalog.max_decoded_bytes must be at least max_download_bytes and at most 67108864"))
	}
	if c.ModelCatalog.MaxCompressionRatio < 1 || c.ModelCatalog.MaxCompressionRatio > 100 {
		problems = append(problems, errors.New("model_catalog.max_compression_ratio must be between 1 and 100"))
	}
	if c.ModelCatalog.MaxEntries < 1 || c.ModelCatalog.MaxEntries > 100000 {
		problems = append(problems, errors.New("model_catalog.max_entries must be between 1 and 100000"))
	}
	if pin := c.ModelCatalog.PinnedRevision; pin != "" {
		if len(pin) != len("sha256:")+64 || !strings.HasPrefix(pin, "sha256:") {
			problems = append(problems, errors.New("model_catalog.pinned_revision must be a sha256 digest"))
		} else if _, err := hex.DecodeString(strings.TrimPrefix(pin, "sha256:")); err != nil {
			problems = append(problems, errors.New("model_catalog.pinned_revision must be a sha256 digest"))
		}
	}
	if region := c.LegacyProviders.Bedrock.Region; region != "" && !validLegacyBedrockRegion(region) {
		problems = append(problems, errors.New("providers.bedrock.region must be an AWS region name such as us-east-1"))
	}
	if c.TLS.Enabled {
		if len(c.TLS.Certificates) == 0 {
			problems = append(problems, errors.New("tls.certificates requires at least one entry when TLS is enabled"))
		}
		if len(c.TLS.Certificates) > MaxTLSCertificates {
			problems = append(problems, fmt.Errorf("tls.certificates accepts at most %d entries", MaxTLSCertificates))
		}
		seen := make(map[string]struct{}, len(c.TLS.Certificates))
		for index, entry := range c.TLS.Certificates {
			if entry.CertFile == "" || entry.KeyFile == "" {
				problems = append(problems, fmt.Errorf("tls.certificates[%d] requires both cert_file and key_file", index))
				continue
			}
			// The same keypair listed twice would build two identical names and
			// be refused later as a duplicate, which reads as a certificate
			// problem rather than a configuration one. Say it here instead.
			if _, exists := seen[entry.CertFile]; exists {
				problems = append(problems, fmt.Errorf("tls.certificates[%d].cert_file is listed more than once", index))
			}
			seen[entry.CertFile] = struct{}{}
		}
	} else if len(c.TLS.Certificates) > 0 {
		problems = append(problems, errors.New("tls.certificates cannot be set while TLS is disabled"))
	}

	if !opts.SkipListenerValidation {
		problems = append(problems, validateListener("server.gateway_listen", c.Server.GatewayListen, c.TLS.Enabled, opts.AllowInsecurePublicGateway)...)
		problems = append(problems, validateListener("server.admin_listen", c.Server.AdminListen, c.TLS.Enabled, false)...)
	}
	if c.Metrics.Enabled {
		metricsTLSEnabled := c.Metrics.TLS.Enabled
		if !opts.SkipListenerValidation {
			problems = append(problems, validateListener("server.metrics_listen", c.Server.MetricsListen, metricsTLSEnabled, false)...)
			metricsHost, _, metricsAddressErr := net.SplitHostPort(c.Server.MetricsListen)
			if metricsAddressErr == nil && !listenerHostIsLoopback(metricsHost) {
				if c.Metrics.CredentialFile == "" {
					problems = append(problems, errors.New("non-loopback metrics listener requires metrics.credential_file"))
				}
				if !c.Metrics.TLS.Enabled {
					problems = append(problems, errors.New("non-loopback metrics listener requires dedicated metrics.tls mutual authentication"))
				}
			}
		}
		if c.Metrics.CredentialFile != "" && !c.Metrics.RequireAuth {
			problems = append(problems, errors.New("metrics.credential_file requires metrics.require_auth"))
		}
		if c.Metrics.MaxConcurrentScrapes < 1 || c.Metrics.MaxConcurrentScrapes > 32 {
			problems = append(problems, errors.New("metrics.max_concurrent_scrapes must be between 1 and 32"))
		}
		if c.Metrics.WriteTimeout <= 0 || c.Metrics.WriteTimeout > Duration(30*time.Second) {
			problems = append(problems, errors.New("metrics.write_timeout must be between zero and 30 seconds"))
		}
		if c.Metrics.TLS.Enabled {
			if c.Metrics.TLS.CertFile == "" || c.Metrics.TLS.KeyFile == "" || c.Metrics.TLS.ClientCAFile == "" {
				problems = append(problems, errors.New("metrics.tls cert_file, key_file, and client_ca_file are required when enabled"))
			}
		} else if c.Metrics.TLS.CertFile != "" || c.Metrics.TLS.KeyFile != "" || c.Metrics.TLS.ClientCAFile != "" {
			problems = append(problems, errors.New("metrics.tls files cannot be set while metrics.tls is disabled"))
		}
	}
	if c.Audit.Anchor.Enabled {
		switch c.Audit.Anchor.Sink {
		case AuditAnchorSinkDeadManPull:
			// The anchor-pull endpoint is served on the metrics listener
			// (ADR 0015): it is already an independent port with a
			// bearer/mTLS story a probe-style caller needs, and standing up
			// a second listener for one more endpoint would duplicate that
			// story rather than reuse it.
			if !c.Metrics.Enabled {
				problems = append(problems, errors.New("audit.anchor.sink dead_man_pull requires metrics.enabled"))
			}
			if c.Audit.Anchor.CredentialFile == "" {
				problems = append(problems, errors.New("audit.anchor.credential_file is required for sink dead_man_pull"))
			}
			// The anchor exists so a witness outside this host can contradict
			// it. Sharing one credential with /metrics collapses that into one
			// domain: whoever scrapes metrics can read the witness feed, and a
			// leak on the scrape path takes the witness with it. The code
			// already keeps two authorizers; nothing until now kept two files.
			if c.Audit.Anchor.CredentialFile != "" && c.Audit.Anchor.CredentialFile == c.Metrics.CredentialFile {
				problems = append(problems, errors.New("audit.anchor.credential_file must differ from metrics.credential_file; the anchor is a separate credential domain"))
			}
			// Anchors and the token that fetches them are the evidence of
			// non-repudiation. Serving them in the clear lets anyone on the
			// path read the chain heads and take the credential.
			if !c.Metrics.TLS.Enabled {
				problems = append(problems, errors.New("audit.anchor.sink dead_man_pull requires metrics.tls.enabled; the anchor feed must not be served in the clear"))
			}
		case AuditAnchorSinkSyslog, AuditAnchorSinkS3ObjectLock:
			problems = append(problems, fmt.Errorf("audit.anchor.sink %q is a reserved name and not implemented yet", c.Audit.Anchor.Sink))
		default:
			problems = append(problems, fmt.Errorf("audit.anchor.sink %q is not a recognized sink", c.Audit.Anchor.Sink))
		}
		if c.Audit.Anchor.Interval <= 0 || c.Audit.Anchor.Interval > Duration(time.Hour) {
			problems = append(problems, errors.New("audit.anchor.interval must be between zero and one hour"))
		}
		if c.Audit.Anchor.RecordDelta < 1 {
			problems = append(problems, errors.New("audit.anchor.record_delta must be at least 1"))
		}
	}
	if !opts.SkipListenerValidation {
		listeners := map[string]string{
			"gateway": c.Server.GatewayListen,
			"admin":   c.Server.AdminListen,
		}
		if c.Metrics.Enabled {
			listeners["metrics"] = c.Server.MetricsListen
		}
		for leftName, leftAddress := range listeners {
			for rightName, rightAddress := range listeners {
				if leftName < rightName && leftAddress == rightAddress {
					problems = append(problems, fmt.Errorf("server %s and %s listeners must be distinct", leftName, rightName))
				}
			}
		}
	}

	if c.Server.ReadHeaderTimeout <= 0 {
		problems = append(problems, errors.New("server.read_header_timeout must be positive"))
	}
	if c.Server.ReadBodyTimeout <= 0 {
		problems = append(problems, errors.New("server.read_body_timeout must be positive"))
	}
	if c.Server.ShutdownTimeout <= 0 {
		problems = append(problems, errors.New("server.shutdown_timeout must be positive"))
	} else if c.Server.ShutdownTimeout < c.Gateway.RouteTotalTimeout {
		problems = append(problems, errors.New("server.shutdown_timeout must be at least gateway.route_total_timeout so accepted requests can drain"))
	}
	if c.Server.MaxHeaderBytes < 1024 {
		problems = append(problems, errors.New("server.max_header_bytes must be at least 1024"))
	}
	if c.Server.MaxRequestBytes <= 0 {
		problems = append(problems, errors.New("server.max_request_bytes must be positive"))
	}
	for name, value := range map[string]Duration{
		"gateway.route_total_timeout":             c.Gateway.RouteTotalTimeout,
		"gateway.attempt_connect_timeout":         c.Gateway.AttemptConnectTimeout,
		"gateway.attempt_response_header_timeout": c.Gateway.AttemptResponseHeaderTimeout,
		"gateway.downstream_write_timeout":        c.Gateway.DownstreamWriteTimeout,
		"gateway.stream_max_duration":             c.Gateway.StreamMaxDuration,
		"gateway.health_probe_interval":           c.Gateway.HealthProbeInterval,
	} {
		if value <= 0 {
			problems = append(problems, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.Gateway.PricingClockRollbackTolerance.Value() < MinPricingClockRollbackTolerance {
		problems = append(problems, fmt.Errorf("gateway.pricing_clock_rollback_tolerance must be at least %s, so concurrent price selections cannot be mistaken for a wall-clock rollback", MinPricingClockRollbackTolerance))
	}
	if c.Gateway.PricingClockForwardTolerance < 0 {
		problems = append(problems, errors.New("gateway.pricing_clock_forward_tolerance cannot be negative"))
	}
	if c.Gateway.PricingUnknownPolicy != "" && c.Gateway.PricingUnknownPolicy != "reject" &&
		c.Gateway.PricingUnknownPolicy != "allow_without_cost_governance" {
		problems = append(problems, errors.New("gateway.pricing_unknown_policy must be reject or allow_without_cost_governance"))
	}
	if c.Gateway.MaxTotalAttempts < 1 {
		problems = append(problems, errors.New("gateway.max_total_attempts must be at least 1"))
	}
	// The ceiling is deliberate rather than arbitrary: every worker can hold one
	// upstream call open for route_total_timeout, so this is how much concurrent
	// upstream work the deferred tier may add on top of the synchronous path.
	if c.Gateway.DeferredResponseWorkers < 0 || c.Gateway.DeferredResponseWorkers > MaxDeferredResponseWorkers {
		problems = append(problems, fmt.Errorf("gateway.deferred_response_workers must be between 1 and %d", MaxDeferredResponseWorkers))
	}
	if c.Gateway.SourceRateLimit.RequestsPerMinute != nil && *c.Gateway.SourceRateLimit.RequestsPerMinute < 0 {
		problems = append(problems, errors.New("gateway.source_rate_limit.requests_per_minute cannot be negative"))
	}
	if c.Gateway.SourceRateLimit.MaxTrackedSources < 0 {
		problems = append(problems, errors.New("gateway.source_rate_limit.max_tracked_sources cannot be negative"))
	}
	problems = append(problems, validateFailureCapture(c.Gateway.FailureCapture)...)
	if c.Retry.MaxAttemptsPerTarget < 1 {
		problems = append(problems, errors.New("retry.max_attempts_per_target must be at least 1"))
	}
	if c.Retry.BaseDelay <= 0 || c.Retry.MaxDelay < c.Retry.BaseDelay {
		problems = append(problems, errors.New("retry delays must be positive and max_delay must be at least base_delay"))
	}
	if c.Routing.AvailabilityFailures < 1 || c.Routing.SuspendFor <= 0 ||
		c.Routing.MaxSuspendFor < c.Routing.SuspendFor || c.Routing.ProbeRequests < 1 {
		problems = append(problems, errors.New(
			"routing values must be positive and max_suspend_for must be at least suspend_for"))
	}
	if c.Alerts.QueueCapacity < 1 || c.Alerts.Workers < 1 || c.Alerts.Timeout <= 0 ||
		c.Alerts.MaxAttempts < 1 || c.Alerts.BaseDelay <= 0 ||
		c.Alerts.MaxDelay < c.Alerts.BaseDelay || c.Alerts.DedupCooldown <= 0 {
		problems = append(problems, errors.New("alerts queue, workers, timeout, attempts, delays, and cooldown must be positive"))
	}
	if c.Usage.Durability != "strict" && c.Usage.Durability != "balanced" {
		problems = append(problems, errors.New("usage.durability must be strict or balanced"))
	}
	if c.Usage.WALQueueCapacity < 1 || c.Usage.AnalyticsQueueCapacity < 1 || c.Usage.WALMaxBatch < 1 ||
		c.Usage.WALMaxBatch > c.Usage.WALQueueCapacity ||
		c.Usage.WALFlushInterval <= 0 {
		problems = append(problems, errors.New(
			"usage WAL queue, max batch, and flush interval must be positive and max batch cannot exceed queue capacity",
		))
	}
	if c.Usage.Timezone == "" {
		problems = append(problems, errors.New("usage.timezone is required"))
	} else if _, err := time.LoadLocation(c.Usage.Timezone); err != nil {
		problems = append(problems, fmt.Errorf("usage.timezone: %w", err))
	}
	if c.Usage.CheckpointInterval <= 0 {
		problems = append(problems, errors.New("usage.checkpoint_interval must be positive"))
	}
	if c.Usage.ParquetInterval <= 0 {
		problems = append(problems, errors.New("usage.parquet_interval must be positive"))
	}
	// The archive's floor is the console's floor, because the window may not
	// exceed the archive and the window may not go below seven days. Allowing a
	// shorter retention left the two constraints with no value that satisfies
	// both — a config that could be written but never started.
	if c.Usage.RetentionDays < MinConsoleWindowDays {
		problems = append(problems, fmt.Errorf(
			"usage.retention_days must be at least %d, because usage.console_window_days may not go below that and may not exceed it",
			MinConsoleWindowDays))
	}
	// Seven is the floor because the overview's own chart reads seven days of
	// hourly buckets and request summaries out of the same aggregate; a shorter
	// window would leave that chart with holes rather than with less history.
	if c.Usage.ConsoleWindowDays < MinConsoleWindowDays {
		problems = append(problems, fmt.Errorf(
			"usage.console_window_days must be at least %d, because the overview reads that many days",
			MinConsoleWindowDays))
	}
	// And it cannot exceed the archive: the console would be promising a
	// history the archive no longer holds, and the window is only safe to trim
	// down to what has been exported.
	if c.Usage.RetentionDays >= MinConsoleWindowDays && c.Usage.ConsoleWindowDays > c.Usage.RetentionDays {
		problems = append(problems, errors.New(
			"usage.console_window_days cannot exceed usage.retention_days"))
	}
	if c.Usage.ExportFormat != UsageExportFormatParquet && c.Usage.ExportFormat != UsageExportFormatNDJSON {
		problems = append(problems, errors.New("usage.export_format must be parquet or ndjson"))
	}
	// Only checked when sealing is on: an operator who leaves it off should not
	// have to hold an opinion about a threshold that governs nothing.
	if c.Ledger.Seal.Enabled && c.Ledger.Seal.MaxActiveBytes < MinLedgerSealMaxActiveBytes {
		problems = append(problems, fmt.Errorf(
			"ledger.seal.max_active_bytes must be at least %d bytes", MinLedgerSealMaxActiveBytes))
	}
	if c.Admin.SessionTTL <= 0 || c.Admin.IdleTimeout <= 0 ||
		c.Admin.IdleTimeout > c.Admin.SessionTTL || c.Admin.LoginRPM < 1 {
		problems = append(problems, errors.New(
			"admin session TTL, idle timeout, and login RPM must be positive; idle timeout cannot exceed TTL",
		))
	}
	if c.Admin.SetupTokenFile != "" && !filepath.IsAbs(c.Admin.SetupTokenFile) {
		problems = append(problems, errors.New("admin.setup_token_file must be an absolute path"))
	}
	if c.Admin.SetupTokenTTL == nil || *c.Admin.SetupTokenTTL <= 0 || *c.Admin.SetupTokenTTL > Duration(MaxAdminSetupTokenTTL) {
		problems = append(problems, errors.New("admin.setup_token_ttl must be positive and no greater than 24h"))
	}
	if c.Admin.DeveloperWorkbench != "enabled" && c.Admin.DeveloperWorkbench != "disabled" {
		return errors.New("admin.developer_workbench must be enabled or disabled")
	}
	if c.Admin.MFAPolicy != AdminMFAPolicyOptional &&
		c.Admin.MFAPolicy != AdminMFAPolicyRequired &&
		c.Admin.MFAPolicy != AdminMFAPolicyAdministratorsRequired {
		return errors.New("admin.mfa_policy must be optional, required, or administrators_required")
	}
	detection := c.Admin.ModelCapabilityDetection
	if detection.FreshTTL <= 0 || detection.Retention < detection.FreshTTL || detection.RefreshCooldown <= 0 ||
		detection.TotalTimeout <= 0 || detection.TotalTimeout > Duration(2*time.Minute) ||
		detection.GlobalConcurrency < 1 || detection.ProviderConcurrency < 1 ||
		detection.ProviderConcurrency > detection.GlobalConcurrency || detection.MaxProviderCalls < 1 || detection.MaxProviderCalls > domain.MaxDetectionProviderCalls ||
		detection.CreateRPM < 1 || detection.CreateRPM > 60 {
		problems = append(problems, errors.New("admin.model_capability_detection limits are invalid"))
	}
	if c.Admin.ExternalOrigin != "" {
		origin, err := url.Parse(c.Admin.ExternalOrigin)
		if err != nil || origin.Scheme != "https" || origin.Host == "" ||
			origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			problems = append(problems, errors.New(
				"admin.external_origin must be an HTTPS origin without path, query, or fragment",
			))
		}
	}
	for _, raw := range c.Security.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(raw); err != nil {
			problems = append(problems, fmt.Errorf("security.trusted_proxy_cidrs %q: %w", raw, err))
		}
	}
	if c.Security.TrustProxyHeaders && len(c.Security.TrustedProxyCIDRs) == 0 {
		problems = append(problems, errors.New("security.trusted_proxy_cidrs is required when proxy headers are trusted"))
	}
	return errors.Join(problems...)
}

func (c Config) MetadataPath() string {
	return filepath.Join(c.Storage.DataDir, c.Storage.MetadataFile)
}

func (c Config) LedgerPath() string {
	return filepath.Join(c.Storage.DataDir, "ledger", "ledger.wal")
}

func (c Config) UsagePath() string {
	return filepath.Join(c.Storage.DataDir, "usage")
}

func (c Config) AuditPath() string {
	return filepath.Join(c.Storage.DataDir, "audit", "audit.log")
}

func (c Config) GovernancePath() string {
	return filepath.Join(c.Storage.DataDir, "governance", "governance.journal")
}

func (c Config) GovernanceExportPath() string {
	return filepath.Join(c.Storage.DataDir, "governance", "export")
}

// validateLogging refuses a logging block that cannot be honoured. The limits
// are validated whether or not a file is written: a value that only becomes
// invalid when the operator later switches output to a file is a trap set for
// the day they most want the log.
func validateLogging(logging Logging) []error {
	var problems []error
	switch strings.ToLower(strings.TrimSpace(logging.Level)) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Errorf("logging.level %q must be debug, info, warn or error", logging.Level))
	}
	switch logging.Format {
	case LogFormatJSON, LogFormatText:
	default:
		problems = append(problems, fmt.Errorf("logging.format %q must be json or text", logging.Format))
	}
	switch logging.Output {
	case LogOutputStderr, LogOutputFile, LogOutputBoth:
	default:
		problems = append(problems, fmt.Errorf("logging.output %q must be stderr, file or both", logging.Output))
	}
	if logging.MaxSizeMB < 1 || logging.MaxSizeMB > 4096 {
		problems = append(problems, errors.New("logging.max_size_mb must be between 1 and 4096"))
	}
	if logging.MaxFiles < 1 || logging.MaxFiles > 100 {
		problems = append(problems, errors.New("logging.max_files must be between 1 and 100"))
	}
	// A path ending in a separator names a directory, and the sink would create
	// the whole thing as a directory and then fail to open a file inside itself.
	if file := strings.TrimSpace(logging.File); file != "" && strings.HasSuffix(file, string(os.PathSeparator)) {
		problems = append(problems, errors.New("logging.file must name a file, not a directory"))
	}
	problems = append(problems, validateErrorFile(logging)...)
	return problems
}

// validateErrorFile refuses an errors-only file that cannot be honoured. Its
// limits are checked whether or not it is enabled, for the same reason the
// ordinary log's are: a value that only becomes invalid on the day it is
// switched on is a trap set for the day it is most wanted.
func validateErrorFile(logging Logging) []error {
	var problems []error
	errorFile := logging.ErrorFile
	// Zero is absence and takes the default; anything else is a decision and is
	// held to the same range as the ordinary log's, switched on or not.
	if errorFile.MaxSizeMB != 0 && (errorFile.MaxSizeMB < 1 || errorFile.MaxSizeMB > 4096) {
		problems = append(problems, errors.New("logging.error_file.max_size_mb must be between 1 and 4096"))
	}
	if errorFile.MaxFiles != 0 && (errorFile.MaxFiles < 1 || errorFile.MaxFiles > 100) {
		problems = append(problems, errors.New("logging.error_file.max_files must be between 1 and 100"))
	}
	file := strings.TrimSpace(errorFile.File)
	if file != "" && strings.HasSuffix(file, string(os.PathSeparator)) {
		problems = append(problems, errors.New("logging.error_file.file must name a file, not a directory"))
	}
	// Two sinks on one path is not a stricter log, it is a corrupted one: each
	// holds its own offset and rotates on its own count, so they overwrite each
	// other's records and rotate mid-file. Both paths default to a name inside
	// the data directory, and those two defaults differ — so this can only be
	// reached by an operator naming one of them, and naming it wrong.
	if file != "" && file == strings.TrimSpace(logging.File) {
		problems = append(problems, errors.New("logging.error_file.file must not be the same path as logging.file"))
	}
	return problems
}

// validateFailureCapture holds the one store that keeps caller-written material
// to bounds it cannot be configured out of.
//
// Zero is absence and takes the default, the same rule the errors-only log file
// uses: this block did not exist until now, so every configuration written
// before it has none, and refusing to start over an absent limit for a feature
// that is switched off would brick an existing data directory. A value an
// operator actually wrote is a decision and is range-checked whether or not
// capture is on, so a limit that only becomes invalid on the day it is switched
// on is caught now.
//
// The retention ceiling is not a performance bound. This is the only store that
// holds prompts, and an instance that keeps them for a year has quietly become
// a different product than the one whose threat model was reviewed.
func validateFailureCapture(capture FailureCapture) []error {
	var problems []error
	if capture.MaxBytes != 0 && (capture.MaxBytes < 1024 || capture.MaxBytes > 1<<20) {
		problems = append(problems, errors.New("gateway.failure_capture.max_bytes must be between 1024 and 1048576"))
	}
	if capture.MaxRecordsPerDay != 0 && (capture.MaxRecordsPerDay < 1 || capture.MaxRecordsPerDay > 1_000_000) {
		problems = append(problems, errors.New("gateway.failure_capture.max_records_per_day must be between 1 and 1000000"))
	}
	if capture.Retain != 0 {
		retain := capture.Retain.Value()
		if retain < time.Hour || retain > 30*24*time.Hour {
			problems = append(problems, errors.New("gateway.failure_capture.retain must be between 1h and 720h"))
		}
	}
	return problems
}

func validateMasterKey(masterKey MasterKey) []error {
	var problems []error
	switch masterKey.Mode {
	case MasterKeyModeFile:
		if masterKey.File == "" {
			problems = append(problems, errors.New("storage.master_key.file is required in file mode"))
		}
		if masterKey.PrimarySlot != "" || masterKey.RecoverySlot != "" ||
			masterKey.StartupDeadline != 0 || masterKey.CallTimeout != 0 || len(masterKey.AllowedKMSKeys) != 0 {
			problems = append(problems, errors.New("storage.master_key key_slots fields cannot be set in file mode"))
		}
	case MasterKeyModeKeySlots:
		if masterKey.File != "" {
			problems = append(problems, errors.New("storage.master_key.file cannot be set in key_slots mode"))
		}
		if strings.TrimSpace(masterKey.PrimarySlot) == "" || strings.TrimSpace(masterKey.RecoverySlot) == "" {
			problems = append(problems, errors.New("storage.master_key primary_slot and recovery_slot are required in key_slots mode"))
		} else if masterKey.PrimarySlot == masterKey.RecoverySlot {
			problems = append(problems, errors.New("storage.master_key primary_slot and recovery_slot must be different"))
		}
		if masterKey.StartupDeadline <= 0 {
			problems = append(problems, errors.New("storage.master_key.startup_deadline must be positive in key_slots mode"))
		}
		if masterKey.CallTimeout <= 0 || masterKey.CallTimeout >= masterKey.StartupDeadline {
			problems = append(problems, errors.New("storage.master_key.call_timeout must be positive and less than startup_deadline"))
		}
		problems = append(problems, validateAllowedKMSKeys(masterKey.AllowedKMSKeys)...)
	default:
		problems = append(problems, errors.New("storage.master_key.mode must be file or key_slots"))
	}
	return problems
}

func validateAllowedKMSKeys(keys []AllowedKMSKey) []error {
	var problems []error
	purposes := map[string]bool{"primary": false, "recovery": false}
	identities := make(map[string]string, len(keys))
	for index, key := range keys {
		prefix := fmt.Sprintf("storage.master_key.allowed_kms_keys[%d]", index)
		if _, ok := purposes[key.Purpose]; !ok {
			problems = append(problems, fmt.Errorf("%s.purpose must be primary or recovery", prefix))
		} else {
			purposes[key.Purpose] = true
		}
		if strings.TrimSpace(key.Provider) == "" {
			problems = append(problems, fmt.Errorf("%s.provider is required", prefix))
		} else if key.Provider != "aws-kms" {
			problems = append(problems, fmt.Errorf("%s.provider is not available in this release", prefix))
		}
		if strings.TrimSpace(key.KeyID) == "" {
			problems = append(problems, fmt.Errorf("%s.key_id is required", prefix))
		}
		if key.Provider == "aws-kms" {
			partition, region, account, resource, ok := parseAWSKMSKeyARN(key.KeyID)
			if !ok || !strings.HasPrefix(partition, "aws") || region != key.Region || account != key.Account ||
				!strings.HasPrefix(resource, "key/") || strings.TrimPrefix(resource, "key/") == "" {
				problems = append(problems, fmt.Errorf("%s.key_id must be a full KMS Key ARN matching region and account", prefix))
			}
			if !validAWSRegion(key.Region) {
				problems = append(problems, fmt.Errorf("%s.region is invalid", prefix))
			}
			if !validAWSAccount(key.Account) {
				problems = append(problems, fmt.Errorf("%s.account must contain exactly 12 digits", prefix))
			}
			if key.Algorithm != "" && key.Algorithm != "SYMMETRIC_DEFAULT" {
				problems = append(problems, fmt.Errorf("%s.algorithm must be SYMMETRIC_DEFAULT", prefix))
			}
		}
		if key.Endpoint != "" {
			endpoint, err := url.Parse(key.Endpoint)
			if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
				(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				problems = append(problems, fmt.Errorf("%s.endpoint must be an HTTPS origin without userinfo, path, query, or fragment", prefix))
			}
		}
		identity := strings.Join([]string{key.Provider, key.Region, key.Account, key.KeyID}, "\x00")
		if previousPurpose, exists := identities[identity]; exists && previousPurpose != key.Purpose {
			problems = append(problems, errors.New("storage.master_key primary and recovery allowlists must not use the same KMS key"))
		} else {
			identities[identity] = key.Purpose
		}
	}
	for purpose, present := range purposes {
		if !present {
			problems = append(problems, fmt.Errorf("storage.master_key.allowed_kms_keys requires a %s entry", purpose))
		}
	}
	return problems
}

func parseAWSKMSKeyARN(value string) (partition, region, account, resource string, ok bool) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "kms" {
		return "", "", "", "", false
	}
	return parts[1], parts[3], parts[4], parts[5], true
}

func validAWSRegion(value string) bool {
	if len(value) < 3 || len(value) > 63 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}

func validAWSAccount(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func cleanAbsolutePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func validateListener(name, address string, tlsEnabled, allowInsecurePublic bool) []error {
	if address == "" {
		return []error{fmt.Errorf("%s is required", name)}
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", name, err)}
	}
	if tlsEnabled || listenerHostIsLoopback(host) {
		return nil
	}
	if allowInsecurePublic && name == "server.gateway_listen" {
		return nil
	}
	return []error{fmt.Errorf("%s must bind loopback unless TLS is enabled", name)}
}

// ListenerHostIsLoopback reports whether a listener host is reachable only from
// this machine. Exported because deployment warnings elsewhere need the same
// answer this package's validation uses, and two implementations would drift.
func ListenerHostIsLoopback(host string) bool { return listenerHostIsLoopback(host) }

func listenerHostIsLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func intPointer(value int) *int { return &value }

func durationPointer(value time.Duration) *Duration {
	wrapped := Duration(value)
	return &wrapped
}

// SourceRequestsPerMinute is the resolved per-source budget. Normalize fills the
// absent case, so callers never have to decide what a missing key meant.
func (s SourceRateLimit) SourceRequestsPerMinute() int {
	if s.RequestsPerMinute == nil {
		return defaultSourceRequestsPerMinute
	}
	return *s.RequestsPerMinute
}

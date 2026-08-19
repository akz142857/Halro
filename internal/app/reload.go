package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

// Reloadable names every item SIGHUP may replace. The list is closed on
// purpose. "Re-read the configuration file" is not what this does, and treating
// it that way would make the relationship between the running process and the
// file on disk unstateable: some keys would be live, others stale, and nothing
// would say which.
//
// The rule that decides membership is that a reload replaces *material* — the
// bytes behind a path, a verbosity — and never *semantics*. Which addresses are
// trusted as proxies, which origin the Admin console is, where the data
// directory is: those decide who is allowed to do what, and changing one is a
// deliberate act that should leave a restart in the record rather than ride
// along on a signal.
const (
	ReloadTLS        = "tls"
	ReloadMetricsTLS = "metrics_tls"
	ReloadLogLevel   = "log_level"
	ReloadLogFile    = "log_file"
)

// reloadItems is the render and iteration order, fixed so the metric label set
// is the same on every scrape whether or not an item has ever run.
var reloadItems = []string{ReloadTLS, ReloadMetricsTLS, ReloadLogLevel, ReloadLogFile}

// LogControls is the part of the process log a reload may touch. It is an
// interface so the runtime does not have to be handed the whole logging
// package, and so tests can observe what a reload did without a real file.
type LogControls interface {
	SetLevel(level slog.Level)
	Level() slog.Level
	HasFile() bool
	ReopenFile() error
}

type reloadItemState struct {
	success         uint64
	failure         uint64
	lastSuccessUnix int64
}

// reloadRuntime is the whole of what SIGHUP touches: the material being served,
// where a new value for the one reloadable scalar is read from, and what the
// last signal did. Grouping it keeps Runtime from growing five more fields for
// one subsystem.
type reloadRuntime struct {
	// serving and metricsTLS are nil when the corresponding listener serves
	// plaintext, which is how the listener setup decides between ServeTLS and
	// Serve.
	serving     *certificateHolder
	metricsTLS  *metricsTLSHolder
	configPath  string
	logControls LogControls
	state       reloadState
}

type reloadState struct {
	mu    sync.Mutex
	items map[string]reloadItemState
}

// record counts one outcome. An item this deployment does not have is not
// recorded at all — "nothing to reload" is neither a success nor a failure, and
// counting it as either would make the metric answer a different question than
// the one an operator is asking.
func (s *reloadState) record(item string, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]reloadItemState, len(reloadItems))
	}
	state := s.items[item]
	if err != nil {
		state.failure++
	} else {
		state.success++
		state.lastSuccessUnix = now.Unix()
	}
	s.items[item] = state
}

func (s *reloadState) snapshot() map[string]reloadItemState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]reloadItemState, len(reloadItems))
	for _, item := range reloadItems {
		out[item] = s.items[item]
	}
	return out
}

// SetReloadSources hands the runtime what it needs to answer a SIGHUP: the path
// the configuration was read from, and the live log. Both are owned by the
// command rather than by Open, and a runtime without them still reloads its
// certificates — which is the part that cannot wait for a restart.
func (r *Runtime) SetReloadSources(configPath string, controls LogControls) {
	r.reload.configPath = configPath
	r.reload.logControls = controls
}

// Reload applies every reloadable item, independently. One item's failure keeps
// that item's previous value and does not stop the others: a broken keypair
// must not also prevent the log level from being lowered while the operator is
// trying to work out what broke.
//
// The returned error is the join of what failed. Callers log it; they must not
// treat it as a reason to stop serving.
func (r *Runtime) Reload() error {
	now := time.Now()
	var problems []error

	apply := func(item string, run func() error) {
		if run == nil {
			return // nothing of this kind is configured; see record's comment
		}
		err := run()
		r.reload.state.record(item, err, now)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", item, err))
			r.logger.Error("reload item failed", "item", item, "error", err)
			return
		}
		r.logger.Info("reload item applied", "item", item)
	}

	// The file is read once, before any item runs, because two things need it
	// and they are not the same thing: the log level is one item's material,
	// while the report of what a reload could *not* apply describes the whole
	// signal. Deriving the second from the first would have made an operator's
	// only notice of a restart-only edit depend on whether this deployment
	// happens to reload a log level at all.
	next, configErr := r.readReloadConfig()

	var reloadServing, reloadMetricsTLS, reloadLevel, reopenLog func() error
	if r.reload.serving != nil {
		reloadServing = r.reload.serving.reload
	}
	if r.reload.metricsTLS != nil {
		reloadMetricsTLS = r.reload.metricsTLS.reload
	}
	// The level is read out of the configuration file, so without a path there
	// is nowhere for a new value to come from.
	if r.reload.logControls != nil && r.reload.configPath != "" {
		reloadLevel = func() error {
			if configErr != nil {
				return configErr
			}
			r.applyLogLevel(next)
			return nil
		}
	}
	if r.reload.logControls != nil && r.reload.logControls.HasFile() {
		reopenLog = r.reload.logControls.ReopenFile
	}

	apply(ReloadTLS, reloadServing)
	apply(ReloadMetricsTLS, reloadMetricsTLS)
	apply(ReloadLogLevel, reloadLevel)
	apply(ReloadLogFile, reopenLog)

	return errors.Join(problems...)
}

// readReloadConfig re-reads the whole configuration file. It is parsed and
// validated in full even though one scalar is all that will be taken from it:
// applying a value out of a file that would be refused on startup would mean
// the running process took a setting from a configuration it cannot actually
// run. Everything outside the allowlist that changed is reported once, because
// the alternative is an operator who edited a key, sent a signal, and has no
// way to learn that the running process still holds the old value.
func (r *Runtime) readReloadConfig() (config.Config, error) {
	if r.reload.configPath == "" {
		return config.Config{}, nil
	}
	next, err := config.Load(r.reload.configPath, config.LoadOptions{
		// The listeners are already bound; re-deciding whether they may bind is
		// not this function's business, and the override flags that governed it
		// belong to the command line rather than the file.
		SkipListenerValidation: true,
	})
	if err != nil {
		return config.Config{}, fmt.Errorf("re-read configuration: %w", err)
	}
	if changed := changedOutsideReloadable(r.config, next); len(changed) > 0 {
		r.logger.Warn("configuration changed in ways a reload cannot apply; restart to take them up",
			"sections", changed)
	}
	return next, nil
}

// applyLogLevel moves the live level and leaves a record of the move.
func (r *Runtime) applyLogLevel(next config.Config) {
	level := next.Logging.SlogLevel()
	previous := r.reload.logControls.Level()
	if level == previous {
		return
	}
	r.reload.logControls.SetLevel(level)
	// Written at whichever of the two levels is more severe, so the record
	// survives under both the setting it left and the setting it arrived at.
	// An Info record written before the change is dropped by exactly the
	// transition that matters most — warn to debug — and a later reader of the
	// verbose stretch is then unable to tell when it began.
	r.logger.Log(context.Background(), max(level, previous, slog.LevelInfo),
		"log level changed", "from", previous.String(), "to", level.String())
}

// effectiveConfig is the configuration as it is actually in force, which is no
// longer the same object as the one that was loaded: the log level may have been
// changed by a reload since. Rendering r.config directly would show an operator
// the file they started with and call it the running state.
func (r *Runtime) effectiveConfig() config.Config {
	effective := r.config
	if r.reload.logControls != nil {
		effective.Logging.Level = logLevelName(r.reload.logControls.Level())
	}
	return effective
}

// logLevelName maps back onto the names config accepts, so a rendered config is
// one the operator could paste back into the file.
func logLevelName(level slog.Level) string {
	switch {
	case level <= slog.LevelDebug:
		return "debug"
	case level <= slog.LevelInfo:
		return "info"
	case level <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

type reloadItemStatus struct {
	Item string `json:"item"`
	// Applies is false for an item this deployment has nothing to reload — no
	// TLS, no log file. Without it, "never succeeded" and "not configured" look
	// identical, and only one of them is worth investigating.
	Applies     bool       `json:"applies"`
	Successes   uint64     `json:"successes"`
	Failures    uint64     `json:"failures"`
	LastSuccess *time.Time `json:"last_success"`
}

type certificateStatus struct {
	Scope    string    `json:"scope"`
	Name     string    `json:"name"`
	NotAfter time.Time `json:"not_after"`
	// Fingerprint answers "which file is actually in force" after a rotation,
	// which is the question the console cannot otherwise settle: the path on
	// disk may have been written over since.
	Fingerprint string `json:"fingerprint"`
}

// reloadStatus is what the console shows so the running state can be compared
// against the file on disk. It answers "when did this last take effect", which
// is the question a configuration that can change without a restart creates.
func (r *Runtime) reloadStatus() map[string]any {
	snapshot := r.reload.state.snapshot()
	applies := map[string]bool{
		ReloadTLS:        r.reload.serving != nil,
		ReloadMetricsTLS: r.reload.metricsTLS != nil,
		ReloadLogLevel:   r.reload.logControls != nil && r.reload.configPath != "",
		ReloadLogFile:    r.reload.logControls != nil && r.reload.logControls.HasFile(),
	}
	items := make([]reloadItemStatus, 0, len(reloadItems))
	for _, item := range reloadItems {
		state := snapshot[item]
		status := reloadItemStatus{
			Item: item, Applies: applies[item],
			Successes: state.success, Failures: state.failure,
		}
		if state.lastSuccessUnix > 0 {
			moment := time.Unix(state.lastSuccessUnix, 0).UTC()
			status.LastSuccess = &moment
		}
		items = append(items, status)
	}
	certificates := make([]certificateStatus, 0, 2)
	if r.reload.serving != nil {
		for _, description := range r.reload.serving.describe() {
			certificates = append(certificates, certificateStatus{
				Scope: "serving", Name: description.Name, NotAfter: description.NotAfter.UTC(),
				Fingerprint: description.Fingerprint,
			})
		}
	}
	if r.reload.metricsTLS != nil {
		for _, description := range r.reload.metricsTLS.describe() {
			certificates = append(certificates, certificateStatus{
				Scope: "metrics", Name: description.Name, NotAfter: description.NotAfter.UTC(),
				Fingerprint: description.Fingerprint,
			})
		}
	}
	return map[string]any{"items": items, "certificates": certificates}
}

// changedOutsideReloadable names the top-level sections that differ once the
// reloadable fields are normalized away. Section names are returned rather than
// values: the configuration carries file paths and listener addresses, and this
// goes to a log.
func changedOutsideReloadable(current, next config.Config) []string {
	current.Logging.Level = ""
	next.Logging.Level = ""
	currentValue, nextValue := reflect.ValueOf(current), reflect.ValueOf(next)
	structType := currentValue.Type()
	var sections []string
	for index := range structType.NumField() {
		if sameConfigSection(currentValue.Field(index).Interface(), nextValue.Field(index).Interface()) {
			continue
		}
		name := structType.Field(index).Tag.Get("yaml")
		if name == "" {
			name = structType.Field(index).Name
		}
		sections = append(sections, name)
	}
	sort.Strings(sections)
	return sections
}

// sameConfigSection compares two sections as the configuration file would spell
// them. reflect.DeepEqual is the wrong instrument here: it separates an absent
// list from an empty one, and a warning that names a section nobody edited
// teaches an operator to stop reading the warning.
func sameConfigSection(current, next any) bool {
	currentBytes, currentErr := yaml.Marshal(current)
	nextBytes, nextErr := yaml.Marshal(next)
	if currentErr != nil || nextErr != nil {
		// A section that cannot be rendered must not silently pass as
		// unchanged; fall back to the structural answer.
		return reflect.DeepEqual(current, next)
	}
	return bytes.Equal(currentBytes, nextBytes)
}

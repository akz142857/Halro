package logging

import (
	"io"
	"log/slog"
	"os"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/safelog"
)

// Bootstrap is the logger a command holds before it has read a configuration
// file — including while it reports that the file could not be read. It is
// deliberately not configurable: there is nothing yet to configure it with.
func Bootstrap() *slog.Logger {
	return safelog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// Open builds the configured logger and returns it with a close function for the
// file sink, if one was opened. The close function is always non-nil, so callers
// can defer it without a branch.
//
// Every logger this returns is wrapped by safelog. Redaction is not a setting:
// the configuration decides where records go and how verbose they are, never
// whether a credential may appear in one.
// Controls is the part of a live logger that may change without rebuilding it:
// how verbose it is, and which file it holds open. Everything else about the
// log — where records go, how they are encoded, what redacts them — is fixed
// when the process starts, because changing it would change what the records
// already written mean.
type Controls struct {
	level *slog.LevelVar
	sink  *Sink
}

// SetLevel changes the minimum severity for records written from now on. The
// underlying LevelVar is read atomically on every record, so this is safe to
// call while the log is in use.
func (c *Controls) SetLevel(level slog.Level) {
	c.level.Set(level)
}

func (c *Controls) Level() slog.Level {
	return c.level.Level()
}

// HasFile reports whether there is a file to reopen. Callers use it to tell
// "nothing to do" apart from "done", so a reload does not claim work it skipped.
func (c *Controls) HasFile() bool {
	return c.sink != nil
}

// ReopenFile reopens the configured log file. See Sink.Reopen for why this
// exists rather than watching the path.
func (c *Controls) ReopenFile() error {
	if c.sink == nil {
		return nil
	}
	return c.sink.Reopen()
}

func (c *Controls) Close() error {
	if c.sink == nil {
		return nil
	}
	return c.sink.Close()
}

func Open(cfg config.Config) (*slog.Logger, *Controls, error) {
	level := new(slog.LevelVar)
	level.Set(cfg.Logging.SlogLevel())
	options := &slog.HandlerOptions{Level: level}
	var writers []io.Writer
	controls := &Controls{level: level}
	if cfg.Logging.WritesStderr() {
		writers = append(writers, os.Stderr)
	}
	if cfg.Logging.WritesFile() {
		sink, err := OpenSink(Options{
			Path:         cfg.LogFilePath(),
			MaxSizeBytes: int64(cfg.Logging.MaxSizeMB) << 20,
			MaxFiles:     cfg.Logging.MaxFiles,
			// A file that cannot be written must not take the log with it, and
			// stderr is the one destination that needs no configuration to work.
			Fallback: os.Stderr,
		})
		if err != nil {
			return nil, controls, err
		}
		writers = append(writers, sink)
		controls.sink = sink
	}
	var destination io.Writer
	switch len(writers) {
	case 0:
		// Unreachable through validated configuration: output is one of three
		// values and every one of them writes somewhere. Discarding is still the
		// right answer to "nowhere" — refusing to start over a log destination
		// would be a worse trade than running quietly.
		destination = io.Discard
	case 1:
		destination = writers[0]
	default:
		destination = io.MultiWriter(writers...)
	}
	var handler slog.Handler
	if cfg.Logging.Format == config.LogFormatText {
		handler = slog.NewTextHandler(destination, options)
	} else {
		handler = slog.NewJSONHandler(destination, options)
	}
	return safelog.New(handler), controls, nil
}

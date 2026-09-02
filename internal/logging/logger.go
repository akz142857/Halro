package logging

import (
	"errors"
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
	// Every file this process holds open for logging. There can be two — the
	// ordinary log and the errors-only copy — and both have to be reopened by
	// one SIGHUP and closed by one shutdown. This was a single *Sink, and
	// adding the second destination as "just another handler" would have left
	// the new file un-rotatable by logrotate and un-closed on exit.
	sinks []*Sink
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
	return len(c.sinks) > 0
}

// ReopenFile reopens every configured log file. See Sink.Reopen for why this
// exists rather than watching the path.
//
// Every sink is attempted even after one fails, and the failures are joined: a
// reload that stopped at the first error would leave the second file pointing
// at a rotated-away inode with nothing saying so.
func (c *Controls) ReopenFile() error {
	var problems []error
	for _, sink := range c.sinks {
		if err := sink.Reopen(); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func (c *Controls) Close() error {
	var problems []error
	for _, sink := range c.sinks {
		if err := sink.Close(); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
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
		controls.sinks = append(controls.sinks, sink)
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
	handlers := []slog.Handler{handler}
	if cfg.Logging.ErrorFile.Enabled {
		errorHandler, errorSink, err := openErrorHandler(cfg)
		if err != nil {
			return nil, controls, err
		}
		handlers = append(handlers, errorHandler)
		controls.sinks = append(controls.sinks, errorSink)
	}
	// One safelog, wrapping the fan-out. Redaction is not a destination's
	// setting: put it the other way round and whether a record is redacted
	// depends on which already-wrapped logger the call site reached for.
	return safelog.New(newFanout(handlers...)), controls, nil
}

// openErrorHandler builds the errors-only destination.
//
// Its threshold is slog.LevelError, fixed — not the LevelVar the ordinary log
// reads. SetLevel is a live control, and a file whose contract is "these are
// the errors" must not quietly start taking INFO because someone turned the
// main log up to debug at three in the morning.
//
// Its encoding is JSON, also fixed. This is the file that gets grepped,
// shipped and pasted into a ticket, and text encoding would make the one
// destination that exists for machines the harder of the two to parse.
func openErrorHandler(cfg config.Config) (slog.Handler, *Sink, error) {
	sink, err := OpenSink(Options{
		Path:         cfg.ErrorLogFilePath(),
		MaxSizeBytes: int64(cfg.Logging.ErrorFile.SizeLimitMB()) << 20,
		MaxFiles:     cfg.Logging.ErrorFile.FileLimit(),
		// The same reason the ordinary log has one: a file that cannot be
		// written must not make the errors silent as well, and the errors are
		// the half nobody can afford to lose.
		Fallback: os.Stderr,
	})
	if err != nil {
		return nil, nil, err
	}
	return slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelError}), sink, nil
}

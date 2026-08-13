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
func Open(cfg config.Config) (*slog.Logger, func() error, error) {
	options := &slog.HandlerOptions{Level: cfg.Logging.SlogLevel()}
	var writers []io.Writer
	closeSink := func() error { return nil }
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
			return nil, closeSink, err
		}
		writers = append(writers, sink)
		closeSink = sink.Close
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
	return safelog.New(handler), closeSink, nil
}

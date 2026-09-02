package domain

import (
	"errors"
	"fmt"
	"time"
)

// How far back the console pages, as a governed setting rather than a file.
//
// It began in config.yaml, which was the wrong home for it: shortening the
// window destroys the attempt history it trims, and a destructive change should
// leave an audit record and a revision behind rather than being whatever the
// file said at the last restart. config.yaml still seeds it once, the same way
// it seeds the accounting timezone, and has no say afterwards.

const (
	// MinInstanceConsoleWindowDays is seven because the overview's own chart
	// reads seven days of hourly buckets out of the same aggregate; a shorter
	// window leaves that chart with holes rather than with less history.
	MinInstanceConsoleWindowDays = 7
	// MaxInstanceConsoleWindowDays is ten years — a ceiling on the number, not
	// a recommendation. What actually bounds a sensible value is the archive's
	// retention and the checkpoint time the window costs, and both are checked
	// where they are known.
	MaxInstanceConsoleWindowDays = 3650
)

// ConsoleWindowPresets are what the console offers. They are presets rather
// than the permitted set: an instance seeded from a config file that said 45
// keeps 45 until someone deliberately changes it.
var ConsoleWindowPresets = []int{30, 60, 90, 180}

type InstanceUsageSettings struct {
	ConsoleWindowDays int       `json:"console_window_days"`
	UpdatedAt         time.Time `json:"updated_at"`
	Revision          uint64    `json:"revision"`
}

func (s *InstanceUsageSettings) GetRevision() uint64      { return s.Revision }
func (s *InstanceUsageSettings) SetRevision(value uint64) { s.Revision = value }

func (s InstanceUsageSettings) Validate() error {
	if s.ConsoleWindowDays < MinInstanceConsoleWindowDays {
		return fmt.Errorf("console window must be at least %d days, because the overview reads that many",
			MinInstanceConsoleWindowDays)
	}
	if s.ConsoleWindowDays > MaxInstanceConsoleWindowDays {
		return errors.New("console window is unreasonably long")
	}
	return nil
}

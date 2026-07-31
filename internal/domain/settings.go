package domain

import (
	"errors"
	"time"
)

// RuntimeSettings contains the deliberately small set of settings that may be
// changed without moving startup security boundaries out of config.yaml.
type RuntimeSettings struct {
	HealthProbeIntervalSeconds int64     `json:"health_probe_interval_seconds"`
	UpdatedAt                  time.Time `json:"updated_at"`
	Revision                   uint64    `json:"revision"`
}

func (s *RuntimeSettings) GetRevision() uint64      { return s.Revision }
func (s *RuntimeSettings) SetRevision(value uint64) { s.Revision = value }

func (s RuntimeSettings) Validate() error {
	if s.HealthProbeIntervalSeconds < 10 || s.HealthProbeIntervalSeconds > 3600 {
		return errors.New("health probe interval must be between 10 and 3600 seconds")
	}
	return nil
}

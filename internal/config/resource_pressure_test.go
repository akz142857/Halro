package config

import (
	"gopkg.in/yaml.v3"
	"strings"
	"testing"
	"time"
)

const pressureConfigFixture = `
resource_pressure:
  runtime_enter_bytes: 100
  runtime_exit_bytes: 80
  cgroup_enter_bytes: 200
  cgroup_exit_bytes: 160
  headroom_enter_bytes: 20
  headroom_exit_bytes: 40
  disk_enter_bytes: 100
  disk_exit_bytes: 200
  inodes_enter: 10
  inodes_exit: 20
  max_age: 10s
  cooldown: 2s
  sample_interval: 1s
`

func TestResourcePressureOptInDecodeValidationAndRoundTrip(t *testing.T) {
	if Default().ResourcePressure != nil {
		t.Fatal("pressure must require explicit calibration")
	}
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	p := cfg.ResourcePressure.Policy()
	if p.RuntimeEnter != 100 || p.CgroupExit != 160 || p.HeadroomEnter != 20 || p.DiskExit != 200 || p.InodesExit != 20 || p.MaxAge != 10*time.Second || p.Cooldown != 2*time.Second {
		t.Fatal(p)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if round.ResourcePressure == nil || *round.ResourcePressure != *cfg.ResourcePressure {
		t.Fatal("round trip lost calibrated policy")
	}
	for _, bad := range []string{"resource_pressure: {}\n", strings.Replace(pressureConfigFixture, "runtime_exit_bytes: 80", "runtime_exit_bytes: 100", 1), strings.Replace(pressureConfigFixture, "sample_interval: 1s", "sample_interval: 10s", 1)} {
		c, err := Decode(strings.NewReader(validConfig + bad))
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Normalize(); err != nil {
			t.Fatal(err)
		}
		if err = c.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "resource_pressure") {
			t.Fatal("uncalibrated policy accepted", err)
		}
	}
	if _, err = Decode(strings.NewReader(validConfig + pressureConfigFixture + "  invented_field: 1\n")); err == nil {
		t.Fatal("unknown pressure key ignored")
	}
}

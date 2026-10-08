package config

import (
	"fmt"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ResourcePressure is an opt-in physical admission guard. Every threshold is
// explicitly calibrated; absent means disabled. It is not a complete logical
// resource budget or permission to skip recovery/settlement durability checks.
type ResourcePressure struct {
	RuntimeEnterBytes  int64    `yaml:"runtime_enter_bytes"`
	RuntimeExitBytes   int64    `yaml:"runtime_exit_bytes"`
	CgroupEnterBytes   int64    `yaml:"cgroup_enter_bytes"`
	CgroupExitBytes    int64    `yaml:"cgroup_exit_bytes"`
	HeadroomEnterBytes int64    `yaml:"headroom_enter_bytes"`
	HeadroomExitBytes  int64    `yaml:"headroom_exit_bytes"`
	DiskEnterBytes     int64    `yaml:"disk_enter_bytes"`
	DiskExitBytes      int64    `yaml:"disk_exit_bytes"`
	InodesEnter        int64    `yaml:"inodes_enter"`
	InodesExit         int64    `yaml:"inodes_exit"`
	MaxAge             Duration `yaml:"max_age"`
	Cooldown           Duration `yaml:"cooldown"`
	SampleInterval     Duration `yaml:"sample_interval"`
}

func (p ResourcePressure) Policy() resourcebudget.PressurePolicy {
	return resourcebudget.PressurePolicy{RuntimeEnter: p.RuntimeEnterBytes, RuntimeExit: p.RuntimeExitBytes, CgroupEnter: p.CgroupEnterBytes, CgroupExit: p.CgroupExitBytes, HeadroomEnter: p.HeadroomEnterBytes, HeadroomExit: p.HeadroomExitBytes, DiskEnter: p.DiskEnterBytes, DiskExit: p.DiskExitBytes, InodesEnter: p.InodesEnter, InodesExit: p.InodesExit, MaxAge: p.MaxAge.Value(), Cooldown: p.Cooldown.Value()}
}
func (p ResourcePressure) Validate() error {
	if _, err := resourcebudget.NewController(p.Policy()); err != nil {
		return fmt.Errorf("all thresholds and durations must be positive, with used-byte exit below enter and available-space exit above enter")
	}
	if p.SampleInterval <= 0 || p.SampleInterval >= p.MaxAge {
		return fmt.Errorf("sample_interval must be positive and below max_age")
	}
	return nil
}

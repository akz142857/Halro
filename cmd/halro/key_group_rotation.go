package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/config"
)

type keyGroupConfigPaths struct {
	names [3]string
	count int
}

func (p *keyGroupConfigPaths) String() string {
	return fmt.Sprintf("%d configured group members", p.count)
}
func (p *keyGroupConfigPaths) Set(value string) error {
	if value == "" || p.count == len(p.names) {
		return errors.New("rotate-group requires two or three complete --member-config paths")
	}
	for _, old := range p.names {
		if old == value {
			return errors.New("duplicate group configuration path")
		}
	}
	p.names[p.count] = value
	p.count++
	return nil
}
func runKeyGroupRotation(arguments []string) error {
	return runKeyGroupRotationWithCommand(arguments, func(ctx context.Context, cfgs []config.Config, key, op string) (app.HAGroupKeyRotationResult, error) {
		if key == "" {
			return app.RotateHAGroupKMSMasterKey(ctx, cfgs, op)
		}
		return app.RotateHAGroupMasterKey(ctx, cfgs, key, op)
	})
}
func runKeyGroupRotationWithCommand(arguments []string, run func(context.Context, []config.Config, string, string) (app.HAGroupKeyRotationResult, error)) error {
	flags := flag.NewFlagSet("key rotate-group", flag.ContinueOnError)
	var paths keyGroupConfigPaths
	flags.Var(&paths, "member-config", "configuration for each stopped HA member (repeat for the complete group)")
	newKey := flags.String("new-key-file", "", "existing 32-byte replacement File DEK shared by the group")
	kms := flags.Bool("kms", false, "generate and wrap the shared replacement DEK using each member configured trusted KMS slots")
	operation := flags.String("operation-id", "", "stable non-secret whole-group operation ID for interruption recovery")
	deadline := flags.Duration("timeout", 10*time.Minute, "finite offline group confirmation/recovery deadline")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || paths.count < 2 || (*newKey == "" && !*kms || *newKey != "" && *kms) || *operation == "" || *deadline <= 0 {
		return errors.New("usage: halro key rotate-group --member-config <primary.yaml> --member-config <replica.yaml> [--member-config <replica2.yaml>] (--new-key-file <path> | --kms) --operation-id <id> [--timeout 10m]")
	}
	var cfgs [3]config.Config
	for n := 0; n < paths.count; n++ {
		cfg, err := config.Load(paths.names[n], config.LoadOptions{})
		if err != nil {
			return err
		}
		if *kms && cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots || !*kms && cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile {
			return errors.New("rotate-group key mode must match every configured member")
		}
		cfgs[n] = cfg
	}
	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()
	result, err := run(ctx, cfgs[:paths.count], *newKey, *operation)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

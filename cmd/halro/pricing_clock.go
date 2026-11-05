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
	"github.com/akz142857/Halro/internal/domain"
)

func runPricingClockRecovery(arguments []string) error {
	flags := flag.NewFlagSet("pricing recover-clock", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "configuration file")
	deployment := flags.String("deployment", "", "one deployment to inspect or recover")
	apply := flags.Bool("apply", false, "apply the explicitly reviewed recovery (server must be stopped)")
	revision := flags.Uint64("revision", 0, "quarantine revision from the review")
	reason := flags.String("reason", "", "clock quarantine reason from the review")
	priceID := flags.String("price-id", "", "effective price ID from the review")
	reviewedAt := flags.String("reviewed-at", "", "UTC server review time in RFC3339Nano; valid for five minutes")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *deployment == "" || flags.NArg() != 0 {
		return errors.New("--deployment is required; stop Halro, inspect, then use --apply with all review fields")
	}
	var expected *domain.PricingClockConfirmation
	if *apply {
		at, err := time.Parse(time.RFC3339Nano, *reviewedAt)
		if err != nil || *revision == 0 || *priceID == "" || !domain.IsClockPricingQuarantine(*reason) {
			return errors.New("--apply requires --revision, --reason, --price-id and --reviewed-at from the reviewed output")
		}
		expected = &domain.PricingClockConfirmation{Revision: *revision, Reason: *reason, PriceID: *priceID, ReviewedAt: at.UTC()}
	} else if *revision != 0 || *priceID != "" || *reason != "" || *reviewedAt != "" {
		return errors.New("review fields require --apply")
	}
	cfg, err := config.Load(*configPath, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return err
	}
	review, err := app.ClockPricingOffline(context.Background(), cfg, *deployment, expected)
	if err != nil {
		return err
	}
	if *apply {
		fmt.Fprintln(os.Stdout, "Clock pricing recovery committed and audited.")
	}
	return json.NewEncoder(os.Stdout).Encode(review)
}

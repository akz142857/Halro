package domain

import "testing"

func TestNormalizeAppearance(t *testing.T) {
	cases := map[string]string{
		"":       AppearanceDark,
		"dark":   AppearanceDark,
		"light":  AppearanceLight,
		"System": AppearanceDark,
		"sepia":  AppearanceDark,
	}
	for input, want := range cases {
		if got := NormalizeAppearance(input); got != want {
			t.Errorf("NormalizeAppearance(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestIsSupportedAppearance(t *testing.T) {
	for _, valid := range []string{AppearanceLight, AppearanceDark} {
		if !IsSupportedAppearance(valid) {
			t.Errorf("expected %q to be supported", valid)
		}
	}
	for _, invalid := range []string{"", "system", "auto", "sepia", "DARK"} {
		if IsSupportedAppearance(invalid) {
			t.Errorf("expected %q to be unsupported", invalid)
		}
	}
}

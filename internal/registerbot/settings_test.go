package registerbot

import "testing"

func TestProxyPoolSettingsDefaultAndSafetyBounds(t *testing.T) {
	settings := DefaultSettings()
	if !settings.ProxyPool.Enabled() || settings.ProxyPool.MaxUsableAccountsPerIP != 3 {
		t.Fatalf("unexpected compatibility defaults: %+v", settings.ProxyPool)
	}
	settings.ProxyPool.Mode = ProxyPoolModeEnforce
	if errValidate := validateSettings(&settings); errValidate != nil {
		t.Fatal(errValidate)
	}
	settings.ProxyPool.MaxUsableAccountsPerIP = 4
	if errValidate := validateSettings(&settings); errValidate == nil {
		t.Fatal("per-IP usable-account limit exceeded the hard maximum")
	}
	settings.ProxyPool.MaxUsableAccountsPerIP = 3
	settings.Gateway.VerifyExitIP = false
	if errValidate := validateSettings(&settings); errValidate == nil {
		t.Fatal("enforced proxy pool accepted unverified exits")
	}
}

func TestFreeAIOnboardingDefaults(t *testing.T) {
	fresh := DefaultFreeAIChannel()
	if fresh.OnboardingRole != "student" || fresh.OnboardingSource != "google" {
		t.Fatal("new channel does not have unattended survey defaults")
	}
	for _, test := range []struct{ name, role, source, wantRole, wantSource string }{
		{"old channel", "", "", "student", "google"},
		{"missing source", "researcher", "", "researcher", "google"},
		{"missing role", "", "github", "student", "github"},
		{"explicit choices", "employee", "friend", "employee", "friend"},
	} {
		t.Run(test.name, func(t *testing.T) {
			set := DefaultSettings()
			set.Channels[0].OnboardingRole = test.role
			set.Channels[0].OnboardingSource = test.source
			set.Normalize()
			if set.Channels[0].OnboardingRole != test.wantRole || set.Channels[0].OnboardingSource != test.wantSource {
				t.Fatal("normalization lost defaults or explicit choices")
			}
		})
	}
}

func TestFreeAIOnboardingDefaultsPersistThroughChannelUpdate(t *testing.T) {
	e := newTestEngine(t, testSettings(), nil)
	settings := e.Settings()
	settings.Channels[0].OnboardingRole, settings.Channels[0].OnboardingSource = "", ""
	if errUpdate := e.UpdateSettings(settings); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	channel := e.Settings().Channels[0]
	if channel.OnboardingRole != "student" || channel.OnboardingSource != "google" {
		t.Fatal("saved empty survey settings were not normalized")
	}
}

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"slices"
	"sort"
	"strings"

	"papio/internal/config"
	"papio/internal/credential"
)

// CredentialView is the daemon's resolved credential snapshot. Doctor consumes
// it without opening a vault, changing configuration, or probing a provider.
// A ready credential means local resolution succeeded, not that the provider
// has accepted it.
type CredentialView interface {
	SourcePolicy(string) config.Source
	InstitutionFor(string) (config.Institution, bool)
	Statuses() []credential.Status
	WebhookConfigured() bool
}

func doctorCredentials(cfg config.Config, views []CredentialView) CredentialView {
	if len(views) != 0 && views[0] != nil {
		return views[0]
	}
	return offlineCredentials{cfg: cfg}
}

// Offline diagnostics can observe legacy literals but cannot establish whether
// a reference resolves. Clearing only copied policies also prevents an invalid
// hand-built config containing both a reference and a literal from claiming
// authenticated capacity through the literal.
type offlineCredentials struct{ cfg config.Config }

func (v offlineCredentials) SourcePolicy(name string) config.Source {
	p := v.cfg.Sources[name]
	if p.CredentialRef != "" {
		p.APIKey, p.ClientID, p.ClientSecret = "", "", ""
	}
	return config.EffectiveSourcePolicy(name, p)
}

func (v offlineCredentials) InstitutionFor(name string) (config.Institution, bool) {
	inst, ok := v.cfg.InstitutionFor(name)
	if inst.DocumentDelivery != nil && inst.DocumentDelivery.CredentialRef != "" {
		dd := *inst.DocumentDelivery
		dd.APIKey = ""
		inst.DocumentDelivery = &dd
	}
	return inst, ok
}

func (v offlineCredentials) WebhookConfigured() bool {
	return v.cfg.Notify.WebhookCredentialRef == "" && strings.TrimSpace(v.cfg.Notify.WebhookURL) != ""
}

func (v offlineCredentials) Statuses() []credential.Status {
	var statuses []credential.Status
	for target, t := range credentialTargets(v.cfg) {
		if t.reference == "" {
			continue
		}
		source := "keyring"
		if strings.HasPrefix(t.reference, "env:") {
			source = "environment"
		}
		statuses = append(statuses, credential.Status{Target: target, Source: source, State: "not_checked"})
	}
	if v.cfg.Agent != nil && v.cfg.Agent.Backend == "typesafe" && v.cfg.Agent.CredentialRef == "" {
		statuses = append(statuses, credential.Status{Target: "agent.typesafe", Source: "legacy", State: "not_checked"})
	}
	return statuses
}

type doctorCredentialTarget struct {
	reference string
	enabled   bool
	required  bool
}

func credentialTargets(cfg config.Config) map[string]doctorCredentialTarget {
	targets := make(map[string]doctorCredentialTarget)
	// Discovery selects its backends independently of acquisition's Enabled
	// flag. Its empty selection uses OpenAlex, matching bootstrap wiring.
	discovery := cfg.Discovery.Sources
	if len(discovery) == 0 {
		discovery = []string{config.SourceOpenAlex}
	}
	for _, name := range []string{config.SourceOpenAlex, config.SourceSemanticScholar, config.SourceCORE, config.SourceCrossrefTDM, config.SourceOpenAIRE} {
		p := cfg.Sources[name]
		targets["sources."+name] = doctorCredentialTarget{p.CredentialRef, p.Enabled || slices.Contains(discovery, name), name == config.SourceCORE || name == config.SourceCrossrefTDM}
	}
	agent := doctorCredentialTarget{required: true}
	if cfg.Agent != nil {
		agent.reference = cfg.Agent.CredentialRef
		agent.enabled = cfg.Agent.Backend == "typesafe"
	}
	targets["agent.typesafe"] = agent
	targets["notify.webhook"] = doctorCredentialTarget{
		cfg.Notify.WebhookCredentialRef,
		cfg.Notify.WebhookCredentialRef != "" || strings.TrimSpace(cfg.Notify.WebhookURL) != "",
		true,
	}
	for _, name := range configuredDocumentDeliveryProfiles(cfg) {
		inst, _ := cfg.InstitutionFor(name)
		dd := inst.DocumentDelivery
		if dd.Kind == "illiad" {
			// Manual prefill is valid without an API key. A selected reference
			// or automatic submission still requires credential resolution.
			required := dd.CredentialRef != "" || dd.SubmitPolicy == "auto_if_unconditional"
			targets[deliveryCredentialTarget(name)] = doctorCredentialTarget{dd.CredentialRef, true, required}
		}
	}
	return targets
}

func deliveryCredentialTarget(name string) string {
	if name == "" || name == "default" {
		return "browser.document_delivery"
	}
	return "browser.resolvers." + name + ".document_delivery"
}

func uncheckedSourceReference(cfg config.Config, name string, views []CredentialView) bool {
	_, offline := doctorCredentials(cfg, views).(offlineCredentials)
	return offline && cfg.Sources[name].CredentialRef != ""
}

func checkCredentials(cfg config.Config, add func(string, string, string, string), views ...CredentialView) {
	view := doctorCredentials(cfg, views)
	_, offline := view.(offlineCredentials)
	statuses := append([]credential.Status(nil), view.Statuses()...)
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Target < statuses[j].Target })
	targets := credentialTargets(cfg)
	for _, resolved := range statuses {
		target, known := targets[resolved.Target]
		if !known {
			continue
		}
		source := resolved.Source
		switch source {
		case "keyring", "environment", "legacy", "none":
		default:
			source = "none"
		}
		state := resolved.State
		// Legacy PAPIO_TYPESAFE_API_KEY="" deliberately disables an enrolled
		// backend. An explicitly empty env reference instead resolves invalid.
		disabledAgent := resolved.Target == "agent.typesafe" && resolved.Reference == "" && source == "environment" && state == "not_configured"
		if resolved.Target == "agent.typesafe" && resolved.Reference == "" && source == "environment" {
			// A nonempty legacy environment override is enrollment even when
			// [agent] is absent, so its resolution failure must stay visible.
			target.enabled = !disabledAgent
		}
		status, remediation := Pass, ""
		switch state {
		case "ready":
			if source == "legacy" {
				remediation = "use papio config credentials migrate to move legacy credentials into the shared store"
			}
		case "not_configured":
			status = Skip
		case "missing", "unavailable", "invalid":
			status = Warn
		case "not_checked":
			if offline {
				status = Skip
				remediation = "run papio doctor with the daemon available, or papio config credentials status, to check resolution"
				break
			}
			fallthrough
		default:
			state, status = "invalid", Warn
		}
		if state != "ready" && state != "not_checked" {
			if target.enabled && target.required {
				status = Fail
			}
			if !target.enabled {
				status = Skip
			}
			if status != Skip {
				remediation = "use papio config credentials status and papio config credentials set " + resolved.Target + "; restart the daemon after changing credentials"
				if state == "unavailable" && source == "keyring" {
					remediation = "run Papio in the signed-in user's session with an unlocked credential store, then use papio config credentials status; an explicit environment reference supports headless use"
				}
			}
		}
		detail := state + " (source: " + source + ")"
		if disabledAgent {
			detail += "; the legacy environment override explicitly disables this backend"
		} else if state == "ready" {
			detail += "; resolved locally, provider authentication has not been tested"
		} else if status == Warn && target.enabled && !target.required {
			if strings.HasPrefix(resolved.Target, "sources.") {
				detail += "; continuing with the supported keyless policy"
			} else {
				detail += "; manual prefill remains available"
			}
		} else if status == Fail {
			detail += "; this integration is unavailable"
		}
		add("credential:"+resolved.Target, status, detail, remediation)
	}
}

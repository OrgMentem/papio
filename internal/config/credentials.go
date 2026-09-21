// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"papio/internal/credential"
)

func sourceCredentialKinds(name string) []credential.Kind {
	switch name {
	case SourceOpenAlex:
		return []credential.Kind{credential.KindOpenAlex}
	case SourceSemanticScholar:
		return []credential.Kind{credential.KindSemanticScholar}
	case SourceCORE:
		return []credential.Kind{credential.KindCORE}
	case SourceCrossrefTDM:
		return []credential.Kind{credential.KindCrossrefTDM}
	case SourceOpenAIRE:
		return []credential.Kind{credential.KindOpenAIREClient, credential.KindOpenAIREToken}
	default:
		return nil
	}
}

// CredentialBindings inventories supported integration targets without accessing
// the OS store or environment. Legacy contains credentials, so callers must not
// log or serialize the bindings. Empty consumers are included for setup/status.
func (c Config) CredentialBindings() []credential.Binding {
	var bindings []credential.Binding
	for _, name := range SourceNames() {
		kinds := sourceCredentialKinds(name)
		if len(kinds) == 0 {
			continue
		}
		s := c.Sources[name]
		b := credential.Binding{Target: "sources." + name, Reference: s.CredentialRef, Kinds: kinds}
		if s.APIKey != "" || s.ClientID != "" || s.ClientSecret != "" {
			kind := kinds[0]
			if name == SourceOpenAIRE && s.ClientID == "" && s.ClientSecret == "" {
				kind = credential.KindOpenAIREToken
			}
			// Preserve all fields: a simultaneous OpenAIRE pair and token is
			// ambiguous and must be rejected, never silently collapsed.
			b.Legacy = credential.Record{Kind: kind, APIKey: s.APIKey, ClientID: s.ClientID, ClientSecret: s.ClientSecret}
		}
		bindings = append(bindings, b)
	}
	agent := credential.Binding{Target: "agent.typesafe", Kinds: []credential.Kind{credential.KindTypeSafe}}
	if c.Agent != nil {
		agent.Reference = c.Agent.CredentialRef
	}
	bindings = append(bindings, agent)
	webhook := credential.Binding{Target: "notify.webhook", Reference: c.Notify.WebhookCredentialRef, Kinds: []credential.Kind{credential.KindWebhook}}
	if c.Notify.WebhookURL != "" || c.Notify.WebhookSecret != "" {
		webhook.Legacy = credential.Record{Kind: credential.KindWebhook, URL: c.Notify.WebhookURL, Bearer: c.Notify.WebhookSecret}
	}
	bindings = append(bindings, webhook)
	addDelivery := func(target string, dd *DocumentDelivery) {
		if dd == nil || dd.Kind != "illiad" {
			return
		}
		b := credential.Binding{Target: target, Reference: dd.CredentialRef, Kinds: []credential.Kind{credential.KindILLiad}}
		if dd.APIKey != "" {
			b.Legacy = credential.Record{Kind: credential.KindILLiad, APIKey: dd.APIKey}
		}
		bindings = append(bindings, b)
	}
	addDelivery("browser.document_delivery", c.Browser.DocumentDelivery)
	for _, name := range slices.Sorted(maps.Keys(c.Browser.Resolvers)) {
		addDelivery("browser.resolvers."+name+".document_delivery", c.Browser.Resolvers[name].DocumentDelivery)
	}
	return bindings
}

// ValidateCredentialMigration rejects credentials whose conversion would lose
// information. Load retains legacy behavior; migration must be explicit about
// malformed pairs, unsupported consumers, and conflicting OpenAIRE forms.
func (c Config) ValidateCredentialMigration() error {
	for _, name := range slices.Sorted(maps.Keys(c.Sources)) {
		s := c.Sources[name]
		if s.APIKey == "" && s.ClientID == "" && s.ClientSecret == "" {
			continue
		}
		if len(sourceCredentialKinds(name)) == 0 {
			return fmt.Errorf("sources.%s has unsupported legacy credentials", name)
		}
		if name == SourceOpenAIRE {
			if s.APIKey != "" && (s.ClientID != "" || s.ClientSecret != "") {
				return errors.New("sources.openaire has both a client credential and an API token; select one before migration")
			}
			if (s.ClientID == "") != (s.ClientSecret == "") {
				return errors.New("sources.openaire client_id and client_secret must be migrated together")
			}
		} else if s.ClientID != "" || s.ClientSecret != "" {
			return fmt.Errorf("sources.%s has unsupported legacy client credentials", name)
		}
	}
	return c.validateCredentialReferences()
}

func (c Config) validateCredentialReferences() error {
	validate := func(target, ref string, mixed bool) error {
		if ref == "" {
			return nil
		}
		if err := credential.ValidateReference(ref); err != nil {
			return fmt.Errorf("%s has an invalid credential reference", target)
		}
		if mixed {
			return fmt.Errorf("%s cannot combine a credential reference with literal credentials", target)
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(c.Sources)) {
		s := c.Sources[name]
		if s.CredentialRef != "" && len(sourceCredentialKinds(name)) == 0 {
			return fmt.Errorf("sources.%s does not support credential_ref", name)
		}
		if err := validate("sources."+name, s.CredentialRef, s.APIKey != "" || s.ClientID != "" || s.ClientSecret != ""); err != nil {
			return err
		}
	}
	if c.Agent != nil {
		if err := validate("agent", c.Agent.CredentialRef, false); err != nil {
			return err
		}
		if c.Agent.CredentialRef != "" && c.Agent.Backend != "typesafe" {
			return errors.New("agent.credential_ref requires backend typesafe")
		}
	}
	if err := validate("notify.webhook", c.Notify.WebhookCredentialRef, c.Notify.WebhookURL != "" || c.Notify.WebhookSecret != ""); err != nil {
		return err
	}
	validateDelivery := func(target string, dd *DocumentDelivery) error {
		if dd == nil || dd.CredentialRef == "" {
			return nil
		}
		if dd.Kind != "illiad" {
			return fmt.Errorf("%s.credential_ref requires kind illiad", target)
		}
		return validate(target, dd.CredentialRef, dd.APIKey != "")
	}
	if err := validateDelivery("browser.document_delivery", c.Browser.DocumentDelivery); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(c.Browser.Resolvers)) {
		if err := validateDelivery("browser.resolvers."+name+".document_delivery", c.Browser.Resolvers[name].DocumentDelivery); err != nil {
			return err
		}
	}
	return nil
}

// WithCredentialReference binds or detaches exactly one catalog target. It
// deep-copies configuration and clears that target's legacy credential fields.
// Detaching the agent also disables enrollment, preventing a fallback to its old
// path-derived credential. No store writes or deletions occur here.
func (c Config) WithCredentialReference(target, ref string) (Config, error) {
	found := false
	for _, b := range c.CredentialBindings() {
		if b.Target == target {
			found = true
			break
		}
	}
	if !found {
		return Config{}, errors.New("unknown credential target")
	}
	if ref != "" {
		if err := credential.ValidateReference(ref); err != nil {
			return Config{}, errors.New("invalid credential reference")
		}
	}
	out := c.clone()
	switch {
	case strings.HasPrefix(target, "sources."):
		name := strings.TrimPrefix(target, "sources.")
		if out.Sources == nil {
			out.Sources = make(map[string]Source)
		}
		s, ok := out.Sources[name]
		if !ok {
			s = defaultSources()[name]
		}
		s.CredentialRef, s.APIKey, s.ClientID, s.ClientSecret = ref, "", "", ""
		out.Sources[name] = s
	case target == "agent.typesafe":
		if ref == "" {
			out.Agent = nil
		} else {
			out.Agent = &Agent{Backend: "typesafe", CredentialRef: ref}
		}
	case target == "notify.webhook":
		out.Notify.WebhookCredentialRef = ref
		out.Notify.WebhookURL, out.Notify.WebhookSecret = "", ""
	case target == "browser.document_delivery":
		out.Browser.DocumentDelivery.CredentialRef, out.Browser.DocumentDelivery.APIKey = ref, ""
	default:
		name := strings.TrimSuffix(strings.TrimPrefix(target, "browser.resolvers."), ".document_delivery")
		dd := out.Browser.Resolvers[name].DocumentDelivery
		dd.CredentialRef, dd.APIKey = ref, ""
	}
	if err := out.validate(); err != nil {
		return Config{}, err
	}
	return out, nil
}

// clone keeps the original profile immutable during a staged edit, including
// validation's normalization of nested maps, pointers, and slices.
func (c Config) clone() Config {
	c.Sources = maps.Clone(c.Sources)
	c.Browser.ExtensionIDs = slices.Clone(c.Browser.ExtensionIDs)
	c.Browser.DocumentDelivery = cloneDocumentDelivery(c.Browser.DocumentDelivery)
	c.Browser.Resolvers = maps.Clone(c.Browser.Resolvers)
	for name, inst := range c.Browser.Resolvers {
		inst.DocumentDelivery = cloneDocumentDelivery(inst.DocumentDelivery)
		c.Browser.Resolvers[name] = inst
	}
	c.Notify.Categories = maps.Clone(c.Notify.Categories)
	c.Library.Sources = slices.Clone(c.Library.Sources)
	c.Discovery.Sources = slices.Clone(c.Discovery.Sources)
	if c.Agent != nil {
		agent := *c.Agent
		c.Agent = &agent
	}
	return c
}

func cloneDocumentDelivery(dd *DocumentDelivery) *DocumentDelivery {
	if dd == nil {
		return nil
	}
	out := *dd
	out.AllowedHosts = slices.Clone(dd.AllowedHosts)
	out.RequestClasses = slices.Clone(dd.RequestClasses)
	return &out
}

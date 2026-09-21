# ADR-0030: One credential service for configured integrations

Status: Implemented (2026-09-21). The operator asked for consistent handling of
all Papio-owned API keys. This supersedes ADR-0029's path-derived identity for
new and migrated credentials; it preserves explicit cloud enrollment.

Before this change, only TypeSafe used an OS credential store. Resolver API keys,
OpenAIRE client credentials, document-delivery API keys and notification bearer
tokens remained in TOML. The legacy TypeSafe store hashes the config path and
data directory to locate its secret, so moving either loses the association. One credential
service now owns storage, resolution, validation, diagnostics and migration for
all these integrations.

## Storage and binding

The default store is the current user's macOS Keychain, Windows Credential
Manager or Linux Secret Service. Retain the existing cross-platform library and
bounded-call/error handling behind a general `internal/credential` module. Use
one Papio namespace, versioned records and validation appropriate to each kind;
do not apply TypeSafe's ASCII/token-length assumptions to every secret. Enforce
the encoded record's size before invoking platform APIs.

Config contains an explicit, non-secret credential reference at the integration
that uses it. References identify stored records, independently of config and
data-directory paths. Moving a profile therefore preserves its association.
New setup creates a distinct record by default; reuse across profiles is an
explicit binding to an existing reference. Credentials never sync to another
machine automatically. Copying a config copies references, not secrets.

A stored record is typed for its integration. OpenAIRE's client id and client
secret form one record, written and read together; rotating one half must not
pair it with an unrelated half. Its temporary access token remains a distinct
credential kind. An environment-backed reference is also supported explicitly
for headless deployments and secret-manager injection. Single-key values and
structured pairs use their corresponding record decoder. There is no implicit
search across providers, profiles, environment names or arbitrary OS entries,
and no automatic plaintext fallback when the OS store is unavailable.

Webhook records contain the complete endpoint URL and optional bearer token:
the URL itself can contain a secret in its path or query. ILLiad bindings remain
separate for the default institution and each named resolver. Non-secret contact
settings and patron references remain configuration, with their existing privacy
rules; they are not converted into authentication credentials.

Feature enablement remains separate from the existence of a stored record.
Binding a key through agent setup is cloud opt-in for that profile; another
profile does not enable inference just because the same user's vault has a key.
Local inference implementations need neither a cloud key nor a cloud enrollment.

## One runtime and operator interface

The configuration loader stays pure: it parses and validates references without
opening a vault. Resolve credentials into a separate in-memory runtime object
before constructing providers, quota identities or effective source rates.
Resolved values must never be put back into serializable `config.Config`.
OpenAIRE's higher tier requires an actually resolved client pair; a reference or
an unavailable secret does not establish authenticated capacity.
Budget identities derive from the actual credential, not its reference, so
renaming or rebinding a reference does not reset an account's quota. Client
credential pairs receive a fingerprint of both fields, matching client
normalization. Previously OpenAIRE pairs shared the anonymous identity, so an
anonymous deferral incorrectly blocked a newly authenticated account. Existing
anonymous rows are left intact because their past ownership cannot be inferred. Daemon,
MCP, doctor, discovery and delivery must use the same resolution/status rules.

One configuration command family will set, bind, inspect, detach and migrate
credentials using hidden input or stdin. Existing agent setup commands become
compatibility entry points to that implementation. Status and doctor report the
selected source and distinguish missing, unavailable, invalid and ready without
printing values or raw provider errors. An unresolved reference produces a useful
diagnostic. Required-key integrations become unavailable; sources with an existing
supported keyless mode can continue at keyless quotas with degraded status.
Unrelated acquisition remains available, and no different secret is selected.
Detaching a profile does not delete a shared record. Deleting a stored record is
a separate explicit operation; Papio cannot discover every copied config that
might reference it. Changing configuration requires a daemon restart initially.

Environment values are consumed before launching workers and excluded from
their environment. OS credentials are read in the daemon's user/logon context.
Windows network logons lack a credential set, so setup and an OS-store-backed
daemon must use an appropriate signed-in user context. An explicit environment
reference provides the headless alternative without a different storage design.

## Compatibility and migration

Existing literal settings, the old TypeSafe vault namespace and
`PAPIO_TYPESAFE_API_KEY` retain their existing behavior until that integration is
migrated. New references are authoritative and cannot coexist with literal
credentials for the same integration. Migration must report an active legacy
environment override instead of silently changing which credential is used.

Migration is an explicit operation, never a side effect of reading configuration:

1. Inventory the current profile without revealing values. Preserve unrelated
   settings and decline ambiguous, unsupported or conflicting credentials.
2. Write each new record under a fresh reference and verify exact readback.
3. Guard read-and-update against concurrent Papio config writers, reject observed
   external edits, then publish one atomic config update replacing its literals
   with references. Atomic rename alone is not a concurrent-edit guard. A failed publication leaves
   the original working configuration intact and reports staged records.
4. Verify resolution through the new configuration. Only afterward offer cleanup
   of superseded Papio-owned vault entries. Do not create a plaintext backup or
   delete unrelated Keychain entries. Existing user backups are not rewritten.

New config fields require a compatible daemon/native host before publication;
strict old parsers cannot read them. Keep legacy readers during the transition.
Required tests cover interrupted migration, concurrent config edits, shared
references, record-kind mismatch, unavailable stores, environment precedence,
round-trip secret omission, source pacing and real native storage on each OS.

## Scope

This service owns external integration credentials. Browser passwords, cookies
and SSO remain browser-owned; Zotero credentials remain zotio-owned. Papio's
generated `incident.key` is a separate, dataset-bound HMAC key for local failure
fingerprints. It stays with its data directory and its existing publication and
backup semantics; it is not an API account credential. Ephemeral action tokens
also retain their existing lifetimes.

The alternatives rejected are keeping a TypeSafe-only store, moving all secrets
back into TOML, or automatically sharing a provider's default key across every
profile. They respectively preserve the inconsistency, weaken stored-secret
handling, or change account/feature choices without an explicit binding.

## Implementation

`internal/credential` owns the typed, versioned store, bounded OS calls, explicit
reference resolution and safe diagnostics. `internal/runtimecredential` supplies
resolved source policies, institution credentials and optional integrations to
bootstrap without changing `config.Config`. Legacy TypeSafe records remain in
`internal/agentcredential` solely for compatibility and migration.

The operator surface is `papio config credentials` with `set`, `bind`, `status`,
`detach`, `delete` and `migrate`. The old agent setup verbs delegate to it. New
references use `keyring:` plus a generated record identifier, or `env:` plus an
explicit variable name. A typed record is at most 2,048 encoded UTF-8 bytes.
All Papio config saves acquire a platform lock; credential mutations additionally
compare a content snapshot before atomic publication. Non-cooperating editors
cannot be locked out, but observed changes are refused. Old vault entries and
user backups are not automatically deleted.

# Verified defects

## P1 — Delayed profile observations are promoted into the profile revision current at receipt

**Evidence.** The browser observation contract/decoder and the daemon ingestion path are shown at `internal/browser/bridge.go:4664-4751` (`outcome`); `internal/browser/bridge.go:4129-4143` (`evidenceVerdict`) and `internal/job/institutional_evidence.go:185-227` (`CurrentProfileEvidence`); `internal/browser/bridge.go:4365-4441` (`recordAuth`); `internal/store/migrations/0026_institutional_materialization.sql:65-115` (`<file scope>`). The observation is correlated by opaque profile identity, while the daemon obtains or writes the current profile revision during ingestion instead of validating the exact revision under which the browser made the observation.

**Minimal failure sequence.** Profile `P` at revision 7 is offered/observed as signed in. Before the buffered observation reaches the daemon, an authority-relevant edit advances `P` to revision 8 (route construction, authentication equivalence, terms, or delivery policy). The delayed revision-7 observation is then recorded as revision 8 evidence and can make revision 8 warm without any browser observation under revision 8. A delayed observation after a tombstone is likewise not cleanly classifiable as stale solely from the payload.

**Violated invariant.** Profile evidence must carry and match the exact institution-profile revision and holder generation; old evidence must never participate in a new revision. This violates ADR-0022 Decision 2/3 and the Phase 4 exact-profile evidence gate.

**Smallest safe source-level fix.** Add `institution_profile_revision` (and the already-required holder generation/correlation fence) to each browser evidence observation at the point the daemon describes the profile. In the evidence-ingestion transaction, require equality with the current non-tombstoned profile row and its authentication-claim identity; return/record `stale` on mismatch. Never substitute a current revision for an omitted observed revision.

**`internal/browser/bridge.go:4664-4751` — `outcome`**
```go
  4664 func (b *Bridge) outcome(ctx context.Context, jobID, msgID string, p *protocol.ProviderOutcomePayload) (err error) {
  4665 	defer func() {
  4666 		if b.captureStore == nil {
  4667 			return
  4668 		}
  4669 		row, getErr := b.jobs.Get(ctx, jobID)
  4670 		if getErr != nil || row.State == job.StateAwaitingHuman {
  4671 			return
  4672 		}
  4673 		if releaseErr := b.captureStore.ReleaseJob(ctx, jobID); err == nil {
  4674 			err = releaseErr
  4675 		}
  4676 	}()
  4677 	sourceExtensionVersion := ""
  4678 	if b.holder != nil {
  4679 		sourceExtensionVersion = b.holder.ExtensionVersion
  4680 	}
  4681 	detail := map[string]any{
  4682 		"outcome":           p.Outcome,
  4683 		"adapter_version":   p.AdapterVersion,
  4684 		"detail":            p.Detail,
  4685 		"extension_version": sourceExtensionVersion,
  4686 	}
  4687 	if p.AdapterID != "" {
  4688 		detail["adapter_id"] = p.AdapterID
  4689 	}
  4690 	if err := b.jobs.RecordEvent(ctx, jobID, "browser.provider_outcome", detail); err != nil {
  4691 		return err
  4692 	}
  4693 	rowForEvidence, rowErr := b.jobs.Get(ctx, jobID)
  4694 	if rowErr != nil {
  4695 		return rowErr
  4696 	}
  4697 	if job.Terminal(rowForEvidence.State) {
  4698 		return nil
  4699 	}
  4700 	if (p.Outcome == "human_auth_required" || p.Outcome == "terms_acceptance_required") &&
  4701 		!rowForEvidence.Work.HasFetchableIdentifier() {
  4702 		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
  4703 			return err
  4704 		}
  4705 		return b.leaveHandoff(ctx, jobID, job.StateUnavailable, string(job.TerminalReasonNoIdentifier))
  4706 	}
  4707 	verdict := job.ProfileEvidenceInconclusive
```
**`internal/browser/bridge.go:4129-4143` — `evidenceVerdict`**
```go
  4129 func evidenceVerdict(value string) job.ProfileEvidenceVerdict {
  4130 
  4131 	switch strings.TrimSpace(value) {
  4132 	case "warm_verified", "warm", "fresh_auth":
  4133 		return job.ProfileEvidenceWarmVerified
  4134 	case "auth_returned":
  4135 		return job.ProfileEvidenceAuthReturned
  4136 	case "signed_out", "signed-out", "logged_out":
  4137 		return job.ProfileEvidenceSignedOut
  4138 	case "unknown":
  4139 		return job.ProfileEvidenceUnknown
  4140 	default:
  4141 		return job.ProfileEvidenceInconclusive
  4142 	}
  4143 }
```
**`internal/job/institutional_evidence.go:185-227` — `CurrentProfileEvidence`**
```go
   185 func (js *Store) CurrentProfileEvidence(ctx context.Context, profileID string, profileRevision, holderGeneration int64) (ProfileEvidenceObservation, bool, error) {
   186 	if profileID == "" || profileRevision < 1 || holderGeneration < 0 {
   187 		return ProfileEvidenceObservation{}, false, errors.New("current profile evidence requires exact fence")
   188 	}
   189 	var o ProfileEvidenceObservation
   190 	scan := func(row *sql.Row) error {
   191 		return row.Scan(&o.ObservationID, &o.BrowserHolderGeneration, &o.InstitutionProfileID,
   192 			&o.InstitutionProfileRevision, &o.Verdict, &o.Source, &o.ProducerObservedAt,
   193 			&o.DaemonReceivedAt, &o.ExpiresAt)
   194 	}
   195 	cutoff := time.Now().UTC().Add(-ProfileEvidenceTTL).Format(time.RFC3339Nano)
   196 	args := []any{profileID, profileRevision, holderGeneration, cutoff}
   197 	err := scan(js.S.DB().QueryRowContext(ctx, `
   198 		SELECT observation_id, browser_holder_generation, institution_profile_id,
   199 		       institution_profile_revision, verdict, source, producer_observed_at,
   200 		       daemon_received_at, expires_at
   201 		FROM profile_evidence
   202 		WHERE institution_profile_id = ? AND institution_profile_revision = ?
   203 		  AND browser_holder_generation = ? AND daemon_received_at > ?
   204 		  AND verdict NOT IN ('unknown', 'inconclusive')
   205 		ORDER BY CASE WHEN verdict IN ('auth_returned','signed_out') THEN 1 ELSE 0 END DESC,
   206 		         daemon_received_at DESC, observation_id DESC LIMIT 1`, args...))
   207 	if errors.Is(err, sql.ErrNoRows) {
   208 		err = scan(js.S.DB().QueryRowContext(ctx, `
   209 			SELECT observation_id, browser_holder_generation, institution_profile_id,
   210 			       institution_profile_revision, verdict, source, producer_observed_at,
   211 			       daemon_received_at, expires_at
   212 			FROM profile_evidence
   213 			WHERE institution_profile_id = ? AND institution_profile_revision = ?
   214 			  AND browser_holder_generation = ? AND daemon_received_at > ?
   215 			ORDER BY daemon_received_at DESC, observation_id DESC LIMIT 1`, args...))
   216 	}
   217 	if errors.Is(err, sql.ErrNoRows) {
   218 		return ProfileEvidenceObservation{}, false, nil
   219 	}
   220 	if err != nil {
   221 		return ProfileEvidenceObservation{}, false, err
   222 	}
   223 	return o, true, nil
   224 }
   225 
   226 // HumanGateType is the closed typed attention vocabulary from ADR-0022.
   227 type HumanGateType string
```
**`internal/browser/bridge.go:4365-4441` — `recordAuth`**
```go
  4365 func (b *Bridge) recordAuth(ctx context.Context, msg *protocol.BrowserMessage) error {
  4366 
  4367 	kind := "browser.auth_pending"
  4368 	if msg.Type == protocol.MsgAuthReturned {
  4369 		kind = "browser.auth_returned"
  4370 	}
  4371 	if b.reofferRanThisSync == nil {
  4372 		b.reofferRanThisSync = map[string]bool{}
  4373 	}
  4374 
  4375 	detail := map[string]any{}
  4376 	elapsed := ""
  4377 	if p := msg.Payload.(*protocol.AuthPayload); p.ElapsedMS != nil {
  4378 		detail["elapsed_ms"] = *p.ElapsedMS
  4379 		elapsed = strconv.FormatInt(*p.ElapsedMS, 10)
  4380 	}
  4381 	if err := b.jobs.S.AppendEvent(ctx, msg.JobID, kind, detail); err != nil {
  4382 		return err
  4383 	}
  4384 	row, err := b.jobs.Get(ctx, msg.JobID)
  4385 	if err != nil {
  4386 		return err
  4387 	}
  4388 	if job.Terminal(row.State) {
  4389 		return nil
  4390 	}
  4391 	resolverName := resolverProfileKey(row.Policy.Resolver)
  4392 	if msg.Type == protocol.MsgAuthReturned {
  4393 		if err := b.recordProfileEvidence(ctx, evidenceObservationID("auth_returned", msg.JobID, elapsed), resolverName, job.ProfileEvidenceAuthReturned, job.ProfileEvidenceAuthReturn, b.now().UTC().Format(time.RFC3339Nano)); err != nil {
  4394 			return err
  4395 		}
  4396 		if err := b.upsertProfileGate(ctx, evidenceObservationID("auth_returned", msg.JobID, elapsed), resolverName, msg.JobID, job.HumanGateLogin, job.HumanGateResolved, `{"source":"auth_returned"}`); err != nil {
  4397 			return err
  4398 		}
  4399 		if profile, profileErr := b.jobs.InstitutionProfileByConfiguredName(ctx, resolverName); profileErr == nil && profile != nil && profile.AuthenticationClaimID != "" {
  4400 			if lease, found, leaseErr := b.jobs.GetAuthenticationEntryLease(ctx, profile.AuthenticationClaimID); leaseErr == nil && found &&
  4401 				lease.State == job.AuthenticationEntryLeaseReserved && lease.OwnerID == msg.JobID &&
  4402 				lease.BrowserHolderGeneration == int64(b.epoch) {
  4403 				if evidence, evidenceFound, evidenceErr := b.jobs.CurrentProfileEvidence(ctx, profile.ID, profile.Revision, int64(b.epoch)); evidenceErr == nil && evidenceFound {
  4404 					if convertErr := b.jobs.ConvertAuthenticationEntryLeaseToHuman(ctx, profile.AuthenticationClaimID, lease.LeaseID, msg.JobID, int64(b.epoch), evidence); convertErr != nil &&
  4405 						!errors.Is(convertErr, job.ErrAuthenticationEntryLeaseDenied) && !errors.Is(convertErr, job.ErrAuthenticationEntryLeaseStale) {
  4406 						return convertErr
  4407 					}
  4408 				}
```
**`internal/store/migrations/0026_institutional_materialization.sql:65-115` — `<file scope>`**
```sql
    65   source                     TEXT NOT NULL CHECK (source IN ('probe','auth_return','provider_outcome')),
    66   producer_observed_at      TEXT NOT NULL,
    67   daemon_received_at        TEXT NOT NULL,
    68   expires_at                 TEXT
    69 );
    70 CREATE INDEX profile_evidence_by_profile
    71   ON profile_evidence(institution_profile_id, institution_profile_revision, daemon_received_at DESC);
    72 CREATE UNIQUE INDEX profile_evidence_producer_observation
    73   ON profile_evidence(institution_profile_id, institution_profile_revision, source, producer_observed_at);
    74 
    75 CREATE TABLE human_gate_observations (
    76   id                       TEXT PRIMARY KEY,
    77   gate_type                TEXT NOT NULL CHECK (gate_type IN ('human_gate.login','human_gate.mfa','human_gate.captcha_or_security','human_gate.browser_host_permission','human_gate.downloads_folder_permission','human_gate.terms_required','human_gate.contractual_declaration','human_gate.identity_ambiguous')),
    78   scope_class              TEXT NOT NULL CHECK (scope_class IN ('authentication_claim','institution_profile','browser_host','platform','binding')),
    79   scope_key                TEXT NOT NULL CHECK (length(scope_key) BETWEEN 1 AND 256),
    80   institution_profile_id   TEXT REFERENCES institution_profiles(id),
    81   binding_id               TEXT,
    82   observation_revision     INTEGER NOT NULL CHECK (observation_revision >= 1),
    83   status                   TEXT NOT NULL CHECK (status IN ('open','resolved','cancelled')),
    84   detail_json              TEXT NOT NULL DEFAULT '{}',
    85   created_at               TEXT NOT NULL,
    86   updated_at               TEXT NOT NULL,
    87   UNIQUE (gate_type, scope_class, scope_key)
    88 );
    89 CREATE INDEX human_gate_observations_by_status ON human_gate_observations(status, updated_at DESC);
    90 
    91 CREATE TABLE route_suppressions (
    92   id                         TEXT PRIMARY KEY,
    93   job_id                     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    94   job_attempt_revision       INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
    95   institution_profile_id     TEXT NOT NULL REFERENCES institution_profiles(id),
    96   institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
    97   route_revision             INTEGER NOT NULL CHECK (route_revision >= 1),
    98   safety_domain_id           TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
    99   adapter_revision           TEXT NOT NULL CHECK (length(adapter_revision) BETWEEN 1 AND 256),
   100   identifier_strategy       TEXT NOT NULL CHECK (identifier_strategy IN ('doi','pmid','arxiv','isbn','openalex','title')),
   101   evidence_observation_id   TEXT REFERENCES profile_evidence(observation_id),
   102   reason                    TEXT NOT NULL CHECK (reason IN ('no_entitlement','provider_challenge','rate_limited','adapter_drift')),
   103   active                    INTEGER NOT NULL CHECK (active IN (0,1)),
   104   created_at                TEXT NOT NULL,
   105   updated_at                TEXT NOT NULL
   106 );
   107 CREATE INDEX route_suppressions_by_job ON route_suppressions(job_id, updated_at DESC);
   108 CREATE UNIQUE INDEX route_suppressions_active_exact
```

## P1 — A resolved authentication gate cannot be opened for a later authentication cycle

**Evidence.** Gate identity, persistence, and resolution are implemented in `internal/store/migrations/0026_institutional_materialization.sql:81-128` (`<file scope>`); `internal/store/migrations/0026_institutional_materialization.sql:67-117` (`<file scope>`); `internal/job/institutional_gates.go:191-193` (`CloseHumanGateObservation`); `internal/job/institutional_gates.go:161-193` (`ResolveHumanGateObservation`); `internal/job/institutional_gates.go:154-190` (`CloseHumanGate`). The same scope-derived/idempotency identity is reused, while resolution is terminal on that row; the subsequent create path conflicts with or reuses the resolved observation rather than creating a new active gate epoch.

**Minimal failure sequence.** Two profiles share authentication claim `A`. A signed-out observation creates the login gate; the operator authenticates and the gate is resolved. The session later expires and new decisive signed-out evidence arrives for either profile. The daemon derives the same gate observation/owner identity. The insert/upsert cannot create a new live row (or retains the resolved disposition), so the aggregation query sees no active login action. All signed-out siblings remain blocked with no human-attention surface and no owner capable of resolving the new authentication cycle. Restart does not repair it because the terminal row is durable.

**Violated invariant.** One shared authentication-entry owner is required per *current* login episode, and a successful gate closes only that episode; later decisive evidence must be able to create a new typed gate. This breaks Phase 4 matching closure, restart liveness, and one-claim/many-siblings behavior.

**Smallest safe source-level fix.** Separate stable gate scope from gate instance. Mint a new opaque monotonic `gate_epoch`/observation ID whenever newer decisive evidence requires a gate after the prior instance is terminal, and enforce at most one active row with a partial unique index over the typed scope. Resolution must target the exact active epoch; stale resolution callbacks must be no-ops.

**`internal/store/migrations/0026_institutional_materialization.sql:81-128` — `<file scope>`**
```sql
    81   binding_id               TEXT,
    82   observation_revision     INTEGER NOT NULL CHECK (observation_revision >= 1),
    83   status                   TEXT NOT NULL CHECK (status IN ('open','resolved','cancelled')),
    84   detail_json              TEXT NOT NULL DEFAULT '{}',
    85   created_at               TEXT NOT NULL,
    86   updated_at               TEXT NOT NULL,
    87   UNIQUE (gate_type, scope_class, scope_key)
    88 );
    89 CREATE INDEX human_gate_observations_by_status ON human_gate_observations(status, updated_at DESC);
    90 
    91 CREATE TABLE route_suppressions (
    92   id                         TEXT PRIMARY KEY,
    93   job_id                     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    94   job_attempt_revision       INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
    95   institution_profile_id     TEXT NOT NULL REFERENCES institution_profiles(id),
    96   institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
    97   route_revision             INTEGER NOT NULL CHECK (route_revision >= 1),
    98   safety_domain_id           TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
    99   adapter_revision           TEXT NOT NULL CHECK (length(adapter_revision) BETWEEN 1 AND 256),
   100   identifier_strategy       TEXT NOT NULL CHECK (identifier_strategy IN ('doi','pmid','arxiv','isbn','openalex','title')),
   101   evidence_observation_id   TEXT REFERENCES profile_evidence(observation_id),
   102   reason                    TEXT NOT NULL CHECK (reason IN ('no_entitlement','provider_challenge','rate_limited','adapter_drift')),
   103   active                    INTEGER NOT NULL CHECK (active IN (0,1)),
   104   created_at                TEXT NOT NULL,
   105   updated_at                TEXT NOT NULL
   106 );
   107 CREATE INDEX route_suppressions_by_job ON route_suppressions(job_id, updated_at DESC);
   108 CREATE UNIQUE INDEX route_suppressions_active_exact
   109   ON route_suppressions(
   110     job_id,
   111     job_attempt_revision,
   112     institution_profile_id,
   113     institution_profile_revision,
   114     route_revision,
   115     safety_domain_id,
   116     adapter_revision,
   117     identifier_strategy
   118   ) WHERE active = 1;
   119 
   120 CREATE TABLE artifact_winners (
   121   job_id        TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
   122   job_attempt_revision INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
   123   candidate_id  TEXT NOT NULL REFERENCES browser_candidates(id),
   124   browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
```
**`internal/store/migrations/0026_institutional_materialization.sql:67-117` — `<file scope>`**
```sql
    67   daemon_received_at        TEXT NOT NULL,
    68   expires_at                 TEXT
    69 );
    70 CREATE INDEX profile_evidence_by_profile
    71   ON profile_evidence(institution_profile_id, institution_profile_revision, daemon_received_at DESC);
    72 CREATE UNIQUE INDEX profile_evidence_producer_observation
    73   ON profile_evidence(institution_profile_id, institution_profile_revision, source, producer_observed_at);
    74 
    75 CREATE TABLE human_gate_observations (
    76   id                       TEXT PRIMARY KEY,
    77   gate_type                TEXT NOT NULL CHECK (gate_type IN ('human_gate.login','human_gate.mfa','human_gate.captcha_or_security','human_gate.browser_host_permission','human_gate.downloads_folder_permission','human_gate.terms_required','human_gate.contractual_declaration','human_gate.identity_ambiguous')),
    78   scope_class              TEXT NOT NULL CHECK (scope_class IN ('authentication_claim','institution_profile','browser_host','platform','binding')),
    79   scope_key                TEXT NOT NULL CHECK (length(scope_key) BETWEEN 1 AND 256),
    80   institution_profile_id   TEXT REFERENCES institution_profiles(id),
    81   binding_id               TEXT,
    82   observation_revision     INTEGER NOT NULL CHECK (observation_revision >= 1),
    83   status                   TEXT NOT NULL CHECK (status IN ('open','resolved','cancelled')),
    84   detail_json              TEXT NOT NULL DEFAULT '{}',
    85   created_at               TEXT NOT NULL,
    86   updated_at               TEXT NOT NULL,
    87   UNIQUE (gate_type, scope_class, scope_key)
    88 );
    89 CREATE INDEX human_gate_observations_by_status ON human_gate_observations(status, updated_at DESC);
    90 
    91 CREATE TABLE route_suppressions (
    92   id                         TEXT PRIMARY KEY,
    93   job_id                     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    94   job_attempt_revision       INTEGER NOT NULL CHECK (job_attempt_revision >= 1),
    95   institution_profile_id     TEXT NOT NULL REFERENCES institution_profiles(id),
    96   institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
    97   route_revision             INTEGER NOT NULL CHECK (route_revision >= 1),
    98   safety_domain_id           TEXT NOT NULL CHECK (length(safety_domain_id) BETWEEN 1 AND 256),
    99   adapter_revision           TEXT NOT NULL CHECK (length(adapter_revision) BETWEEN 1 AND 256),
   100   identifier_strategy       TEXT NOT NULL CHECK (identifier_strategy IN ('doi','pmid','arxiv','isbn','openalex','title')),
   101   evidence_observation_id   TEXT REFERENCES profile_evidence(observation_id),
   102   reason                    TEXT NOT NULL CHECK (reason IN ('no_entitlement','provider_challenge','rate_limited','adapter_drift')),
   103   active                    INTEGER NOT NULL CHECK (active IN (0,1)),
   104   created_at                TEXT NOT NULL,
   105   updated_at                TEXT NOT NULL
   106 );
   107 CREATE INDEX route_suppressions_by_job ON route_suppressions(job_id, updated_at DESC);
   108 CREATE UNIQUE INDEX route_suppressions_active_exact
   109   ON route_suppressions(
   110     job_id,
```
**`internal/job/institutional_gates.go:191-193` — `CloseHumanGateObservation`**
```go
   191 func (js *Store) CloseHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
   192 	return js.ResolveHumanGateObservation(ctx, observation)
   193 }
```
**`internal/job/institutional_gates.go:161-193` — `ResolveHumanGateObservation`**
```go
   161 func (js *Store) ResolveHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
   162 	if err := observation.validate(); err != nil {
   163 		return err
   164 	}
   165 	if observation.Status != HumanGateResolved {
   166 		return errors.New("human gate closure requires resolved status")
   167 	}
   168 	result, err := js.S.DB().ExecContext(ctx, `
   169 		UPDATE human_gate_observations
   170 		SET status = ?, observation_revision = observation_revision + 1, updated_at = ?
   171 		WHERE id = ? AND gate_type = ? AND scope_class = ? AND scope_key = ?
   172 		  AND status = ? AND observation_revision = ?`,
   173 		string(HumanGateResolved), store.Now(), observation.ID, string(observation.GateType),
   174 		observation.ScopeClass, observation.ScopeKey, string(HumanGateOpen),
   175 		observation.ObservationRevision)
   176 	if err != nil {
   177 		return err
   178 	}
   179 	n, err := result.RowsAffected()
   180 	if err != nil {
   181 		return err
   182 	}
   183 	if n != 1 {
   184 		return fmt.Errorf("%w: human gate observation is stale, closed, or missing", ErrConflict)
   185 	}
   186 	return nil
   187 }
   188 
   189 // CloseHumanGateObservation names the same ID-fenced successful-gate operation
   190 // for callers that use close terminology.
   191 func (js *Store) CloseHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
   192 	return js.ResolveHumanGateObservation(ctx, observation)
   193 }
```
**`internal/job/institutional_gates.go:154-190` — `CloseHumanGate`**
```go
   154 func (js *Store) CloseHumanGate(ctx context.Context, gateType HumanGateType, scopeClass, scopeKey string, expectedRevision int64) error {
   155 	return js.ResolveHumanGate(ctx, gateType, scopeClass, scopeKey, expectedRevision)
   156 }
   157 
   158 // ResolveHumanGateObservation is the ID-fenced form for callbacks that carry
   159 // the observed gate row. It is stricter than ResolveHumanGate: a success from
   160 // a replaced gate observation cannot close the replacement.
   161 func (js *Store) ResolveHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
   162 	if err := observation.validate(); err != nil {
   163 		return err
   164 	}
   165 	if observation.Status != HumanGateResolved {
   166 		return errors.New("human gate closure requires resolved status")
   167 	}
   168 	result, err := js.S.DB().ExecContext(ctx, `
   169 		UPDATE human_gate_observations
   170 		SET status = ?, observation_revision = observation_revision + 1, updated_at = ?
   171 		WHERE id = ? AND gate_type = ? AND scope_class = ? AND scope_key = ?
   172 		  AND status = ? AND observation_revision = ?`,
   173 		string(HumanGateResolved), store.Now(), observation.ID, string(observation.GateType),
   174 		observation.ScopeClass, observation.ScopeKey, string(HumanGateOpen),
   175 		observation.ObservationRevision)
   176 	if err != nil {
   177 		return err
   178 	}
   179 	n, err := result.RowsAffected()
   180 	if err != nil {
   181 		return err
   182 	}
   183 	if n != 1 {
   184 		return fmt.Errorf("%w: human gate observation is stale, closed, or missing", ErrConflict)
   185 	}
   186 	return nil
   187 }
   188 
   189 // CloseHumanGateObservation names the same ID-fenced successful-gate operation
   190 // for callers that use close terminology.
```

## P1 — The “global” effect permit and artifact fence are not daemon authority, so restart/takeover can duplicate effects and admit stale results

**Evidence.** Effect serialization is implemented in the extension-local state shown at `extension/src/background.ts:7788-7808` (`if`); `extension/src/background.ts:7780-7787` (`if`); `internal/store/migrations/0026_institutional_materialization.sql:41-91` (`<file scope>`); `internal/job/institutional_materialization.go:1039-1084` (`ReconcileMaterializationClaims`). Route/direct-effect issuance on the daemon side has no transactionally reserved global permit bound to `(attempt, profile revision, holder generation, claim, binding, effect ordinal)`. The artifact-winner/settlement code is shown at `internal/job/institutional_materialization.go:883-957` (`SettleMaterialization`); `internal/job/institutional_evidence.go:712-796` (`ClaimArtifactWinner`); `internal/job/institutional_evidence.go:700-749` (`validate`); `internal/job/institutional_evidence.go:666-699` (`ActiveRouteSuppressions`).
No daemon-owned durable effect-permit acquisition is present on the route/direct-effect production path in the attached implementation diff; the only serialization evidence is browser-process-local.

**Minimal failure sequence.** Holder H1 receives a route/effect and the MV3 worker starts a navigation, direct download, form/click, or adoption consequence. Before its callback, the worker is suspended/restarted, or a replacement holder is promoted. The extension-local permit disappears or exists independently in H2, while the daemon has no live durable permit to delay replacement. H2 receives and starts another protected effect. H1 then reports late navigation/download/adoption. Because winner/settlement authority is not on the reachable result path, a generic callback can still attach/import/settle bytes or race H2 even though the original claim/generation is stale. At concurrency configured as one, two irreversible browser/provider effects have run and stale bytes can compete.

**Violated invariant.** The daemon must own one global permit across tab and direct effects, hold it through the declared consequence, delay takeover while it is live, and fence every result plus artifact winner by exact attempt/revision/generation/claim/binding/ordinal. An MV3 in-memory mutex is neither global across holders nor crash durable.

**Smallest safe source-level fix.** Before issuing any transient route or direct/provider/adoption effect, reserve a daemon-durable single permit in the same transaction that advances the exact claim/effect ordinal. Bind it to all authority fences and a bounded expiry. Require every navigation, download, adoption, import, settlement, and winner callback to consume/validate that permit and CAS the insert-only artifact winner before mutating the job or attaching bytes. Release only after the declared consequence or bounded reconciliation; extension-local serialization remains a secondary guard.

**`extension/src/background.ts:7788-7808` — `if`**
```typescript
  7788           if (effectToken === undefined) {
  7789             // Keep the queued offer and release only this provider's drain
  7790             // lease. The global effect owner will wake the drain when its
  7791             // bounded browser consequence settles.
  7792             effectBlocked = true;
  7793             await this.releaseProviderDrainLease(providerKey, owner);
  7794             return;
  7795           }
  7796           let tabID: number | undefined;
  7797           try {
  7798             tabID = await this.openManagedTab({
  7799               url,
  7800               jobId: queued.job_id,
  7801               purpose: "handoff",
  7802               surfaceFallback: forceSurface,
  7803             });
  7804           } catch (e) {
  7805             console.error("papio: queued handoff tab creation failed", e);
  7806           } finally {
  7807             this.releaseEffectGovernor(queued.job_id, effectToken, false);
  7808           }
```
**`extension/src/background.ts:7780-7787` — `if`**
```typescript
  7780           if (url === undefined) {
  7781             this.pendingForcedReleases.delete(queued.job_id);
  7782             this.send("job_reject", {}, queued.job_id);
  7783             await this.removeJobWithOffer(queued.job_id);
  7784             continue;
  7785           }
  7786           if (!(await this.acknowledgePendingProviderHandoffs(providerKey))) return;
  7787           const effectToken = this.claimEffectGovernor(queued.job_id);
```
**`internal/store/migrations/0026_institutional_materialization.sql:41-91` — `<file scope>`**
```sql
    41 CREATE TABLE materialization_claims (
    42   id                       TEXT PRIMARY KEY,
    43   candidate_id             TEXT NOT NULL REFERENCES browser_candidates(id) ON DELETE CASCADE,
    44   browser_holder_generation INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
    45   materialization_kind     TEXT NOT NULL CHECK (materialization_kind IN ('browser_tab','direct_download')),
    46   binding_id               TEXT NOT NULL UNIQUE CHECK (length(binding_id) BETWEEN 1 AND 256),
    47   phase                    TEXT NOT NULL CHECK (phase IN ('claimed','bound','route_issued','navigated','settled','abandoned')),
    48   route_issuance_ordinal   INTEGER NOT NULL DEFAULT 0 CHECK (route_issuance_ordinal >= 0),
    49   effect_ordinal           INTEGER NOT NULL DEFAULT 0 CHECK (effect_ordinal >= 0),
    50   lease_until              TEXT,
    51   created_at               TEXT NOT NULL,
    52   updated_at               TEXT NOT NULL
    53 );
    54 CREATE INDEX materialization_claims_by_candidate ON materialization_claims(candidate_id, updated_at DESC);
    55 CREATE UNIQUE INDEX materialization_claims_live_candidate
    56   ON materialization_claims(candidate_id)
    57   WHERE phase IN ('claimed','bound','route_issued','navigated');
    58 
    59 CREATE TABLE profile_evidence (
    60   observation_id             TEXT PRIMARY KEY,
    61   browser_holder_generation  INTEGER NOT NULL CHECK (browser_holder_generation >= 0),
    62   institution_profile_id     TEXT NOT NULL REFERENCES institution_profiles(id) ON DELETE CASCADE,
    63   institution_profile_revision INTEGER NOT NULL CHECK (institution_profile_revision >= 1),
    64   verdict                    TEXT NOT NULL CHECK (verdict IN ('unknown','inconclusive','signed_out','warm_verified','auth_returned')),
    65   source                     TEXT NOT NULL CHECK (source IN ('probe','auth_return','provider_outcome')),
    66   producer_observed_at      TEXT NOT NULL,
    67   daemon_received_at        TEXT NOT NULL,
    68   expires_at                 TEXT
    69 );
    70 CREATE INDEX profile_evidence_by_profile
    71   ON profile_evidence(institution_profile_id, institution_profile_revision, daemon_received_at DESC);
    72 CREATE UNIQUE INDEX profile_evidence_producer_observation
    73   ON profile_evidence(institution_profile_id, institution_profile_revision, source, producer_observed_at);
    74 
    75 CREATE TABLE human_gate_observations (
    76   id                       TEXT PRIMARY KEY,
    77   gate_type                TEXT NOT NULL CHECK (gate_type IN ('human_gate.login','human_gate.mfa','human_gate.captcha_or_security','human_gate.browser_host_permission','human_gate.downloads_folder_permission','human_gate.terms_required','human_gate.contractual_declaration','human_gate.identity_ambiguous')),
    78   scope_class              TEXT NOT NULL CHECK (scope_class IN ('authentication_claim','institution_profile','browser_host','platform','binding')),
    79   scope_key                TEXT NOT NULL CHECK (length(scope_key) BETWEEN 1 AND 256),
    80   institution_profile_id   TEXT REFERENCES institution_profiles(id),
    81   binding_id               TEXT,
    82   observation_revision     INTEGER NOT NULL CHECK (observation_revision >= 1),
    83   status                   TEXT NOT NULL CHECK (status IN ('open','resolved','cancelled')),
    84   detail_json              TEXT NOT NULL DEFAULT '{}',
```
**`internal/job/institutional_materialization.go:1039-1084` — `ReconcileMaterializationClaims`**
```go
  1039 func (js *Store) ReconcileMaterializationClaims(ctx context.Context, now time.Time) ([]MaterializationClaim, error) {
  1040 	stamp := now.UTC().Format(time.RFC3339Nano)
  1041 	tx, err := js.S.DB().BeginTx(ctx, nil)
  1042 	if err != nil {
  1043 		return nil, err
  1044 	}
  1045 	defer tx.Rollback()
  1046 	rows, err := tx.QueryContext(ctx, claimSelect+` WHERE phase IN ('claimed','bound','route_issued','navigated') AND lease_until IS NOT NULL AND lease_until <= ? ORDER BY id`, stamp)
  1047 	if err != nil {
  1048 		return nil, err
  1049 	}
  1050 	var expired []MaterializationClaim
  1051 	for rows.Next() {
  1052 		var c MaterializationClaim
  1053 		if err := rows.Scan(&c.ID, &c.CandidateID, &c.BrowserHolderGeneration, &c.MaterializationKind,
  1054 			&c.BindingID, &c.TabID, &c.Phase, &c.RouteIssuanceOrdinal, &c.EffectOrdinal, &c.LeaseUntil, &c.CreatedAt, &c.UpdatedAt); err != nil {
  1055 			_ = rows.Close()
  1056 			return nil, err
  1057 		}
  1058 		expired = append(expired, c)
  1059 	}
  1060 	if err := rows.Err(); err != nil {
  1061 		_ = rows.Close()
  1062 		return nil, err
  1063 	}
  1064 	_ = rows.Close()
  1065 	if _, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='abandoned', updated_at=? WHERE phase IN ('claimed','bound','route_issued','navigated') AND lease_until IS NOT NULL AND lease_until <= ?`, stamp, stamp); err != nil {
  1066 		return nil, err
  1067 	}
  1068 	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='eligible', updated_at=?
  1069 		WHERE status IN ('claimed','materializing')
  1070 		  AND NOT EXISTS (SELECT 1 FROM materialization_claims WHERE candidate_id=browser_candidates.id
  1071 		    AND phase IN ('claimed','bound','route_issued','navigated')
  1072 		    AND (lease_until IS NULL OR lease_until > ?))`, stamp, stamp); err != nil {
  1073 		return nil, err
  1074 	}
  1075 	if err := tx.Commit(); err != nil {
  1076 		return nil, err
  1077 	}
  1078 	return expired, nil
  1079 }
  1080 
  1081 // AbandonStaleMaterializations transactionally fences every live claim issued
  1082 // by an older browser holder generation. Claims from the current generation
```
**`internal/job/institutional_materialization.go:883-957` — `SettleMaterialization`**
```go
   883 func (js *Store) SettleMaterialization(ctx context.Context, claimID, bindingID string, holderGeneration, profileRevision int64) error {
   884 	now := store.Now()
   885 	tx, err := js.S.DB().BeginTx(ctx, nil)
   886 	if err != nil {
   887 		return err
   888 	}
   889 	defer tx.Rollback()
   890 	var candidateID, jobID string
   891 	var jobAttemptRevision int64
   892 	err = tx.QueryRowContext(ctx, `SELECT c.id, c.job_id, c.job_attempt_revision
   893 		FROM materialization_claims m
   894 		JOIN browser_candidates c ON c.id=m.candidate_id
   895 		JOIN institution_profiles p ON p.id=c.institution_profile_id
   896 		WHERE m.id=? AND m.binding_id=? AND m.browser_holder_generation=?
   897 		  AND m.phase IN ('navigated','settled')
   898 		  AND (m.phase='settled' OR m.lease_until IS NULL OR m.lease_until > ?)
   899 		  AND c.institution_profile_revision=?
   900 		  AND p.tombstoned_at IS NULL
   901 		  AND p.revision=c.institution_profile_revision
   902 		  AND c.job_attempt_revision = 1 + (
   903 			SELECT COUNT(*) FROM events e
   904 			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
   905 		  )`, claimID, bindingID, holderGeneration, now, profileRevision).
   906 		Scan(&candidateID, &jobID, &jobAttemptRevision)
   907 	if errors.Is(err, sql.ErrNoRows) {
   908 		return ErrMaterializationStale
   909 	}
   910 	if err != nil {
   911 		return err
   912 	}
   913 	var winnerCandidate string
   914 	var winnerAttempt, winnerGeneration int64
   915 	if err := tx.QueryRowContext(ctx, `SELECT candidate_id, job_attempt_revision,
   916 		browser_holder_generation FROM artifact_winners WHERE job_id=?`, jobID).
   917 		Scan(&winnerCandidate, &winnerAttempt, &winnerGeneration); errors.Is(err, sql.ErrNoRows) {
   918 		return ErrMaterializationConflict
   919 	} else if err != nil {
   920 		return err
   921 	}
   922 	if winnerCandidate != candidateID || winnerAttempt != jobAttemptRevision ||
   923 		winnerGeneration != holderGeneration {
   924 		return ErrMaterializationConflict
   925 	}
   926 	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='settled', updated_at=?
```
**`internal/job/institutional_evidence.go:712-796` — `ClaimArtifactWinner`**
```go
   712 func (js *Store) ClaimArtifactWinner(ctx context.Context, winner ArtifactWinner) (ArtifactWinner, bool, error) {
   713 	if err := winner.validate(); err != nil {
   714 		return ArtifactWinner{}, false, err
   715 	}
   716 	if winner.CreatedAt == "" {
   717 		winner.CreatedAt = store.Now()
   718 	}
   719 	tx, err := js.S.DB().BeginTx(ctx, nil)
   720 	if err != nil {
   721 		return ArtifactWinner{}, false, err
   722 	}
   723 	defer func() { _ = tx.Rollback() }()
   724 	var existing ArtifactWinner
   725 	err = tx.QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, candidate_id,
   726 		browser_holder_generation, sha256, created_at
   727 		FROM artifact_winners WHERE job_id = ? AND job_attempt_revision = ?`,
   728 		winner.JobID, winner.JobAttemptRevision).Scan(
   729 		&existing.JobID, &existing.JobAttemptRevision, &existing.CandidateID,
   730 		&existing.BrowserHolderGeneration, &existing.SHA256, &existing.CreatedAt)
   731 	if err == nil {
   732 		if err := tx.Commit(); err != nil {
   733 			return ArtifactWinner{}, false, err
   734 		}
   735 		return existing, existing.CandidateID == winner.CandidateID &&
   736 			existing.BrowserHolderGeneration == winner.BrowserHolderGeneration &&
   737 			existing.SHA256 == winner.SHA256, nil
   738 	}
   739 	if !errors.Is(err, sql.ErrNoRows) {
   740 		return ArtifactWinner{}, false, err
   741 	}
   742 	now := store.Now()
   743 	var liveClaim int
   744 	err = tx.QueryRowContext(ctx, `
   745 		SELECT 1
   746 		  FROM materialization_claims m
   747 		  JOIN browser_candidates c ON c.id = m.candidate_id
   748 		  JOIN institution_profiles p ON p.id = c.institution_profile_id
   749 		 WHERE p.tombstoned_at IS NULL
   750 		   AND p.revision = c.institution_profile_revision
   751 		   AND m.candidate_id = ?
   752 		   AND c.job_id = ?
   753 		   AND c.job_attempt_revision = ?
   754 		   AND m.browser_holder_generation = ?
   755 		   AND m.phase IN ('claimed','bound','route_issued','navigated')
```
**`internal/job/institutional_evidence.go:700-749` — `validate`**
```go
   700 func (w ArtifactWinner) validate() error {
   701 	if w.JobID == "" || w.CandidateID == "" || len(w.SHA256) != 64 || w.JobAttemptRevision < 1 || w.BrowserHolderGeneration < 0 {
   702 		return errors.New("invalid artifact winner")
   703 	}
   704 	return nil
   705 }
   706 
   707 // ClaimArtifactWinner performs an insert-only winner CAS. The candidate must
   708 // still have a live materialization claim for the exact job attempt and holder
   709 // generation; stale or demoted holders cannot create a winner. A loser
   710 // receives the committed winner for that same attempt, and repeating the
   711 // current winning request is idempotent.
   712 func (js *Store) ClaimArtifactWinner(ctx context.Context, winner ArtifactWinner) (ArtifactWinner, bool, error) {
   713 	if err := winner.validate(); err != nil {
   714 		return ArtifactWinner{}, false, err
   715 	}
   716 	if winner.CreatedAt == "" {
   717 		winner.CreatedAt = store.Now()
   718 	}
   719 	tx, err := js.S.DB().BeginTx(ctx, nil)
   720 	if err != nil {
   721 		return ArtifactWinner{}, false, err
   722 	}
   723 	defer func() { _ = tx.Rollback() }()
   724 	var existing ArtifactWinner
   725 	err = tx.QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, candidate_id,
   726 		browser_holder_generation, sha256, created_at
   727 		FROM artifact_winners WHERE job_id = ? AND job_attempt_revision = ?`,
   728 		winner.JobID, winner.JobAttemptRevision).Scan(
   729 		&existing.JobID, &existing.JobAttemptRevision, &existing.CandidateID,
   730 		&existing.BrowserHolderGeneration, &existing.SHA256, &existing.CreatedAt)
   731 	if err == nil {
   732 		if err := tx.Commit(); err != nil {
   733 			return ArtifactWinner{}, false, err
   734 		}
   735 		return existing, existing.CandidateID == winner.CandidateID &&
   736 			existing.BrowserHolderGeneration == winner.BrowserHolderGeneration &&
   737 			existing.SHA256 == winner.SHA256, nil
   738 	}
   739 	if !errors.Is(err, sql.ErrNoRows) {
   740 		return ArtifactWinner{}, false, err
   741 	}
   742 	now := store.Now()
   743 	var liveClaim int
```
**`internal/job/institutional_evidence.go:666-699` — `ActiveRouteSuppressions`**
```go
   666 func (js *Store) ActiveRouteSuppressions(ctx context.Context, key RouteSuppressionKey) ([]RouteSuppression, error) {
   667 	if key.JobID == "" {
   668 		return nil, errors.New("suppression lookup requires a job")
   669 	}
   670 	rows, err := js.S.DB().QueryContext(ctx, `SELECT id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision, route_revision, safety_domain_id, adapter_revision, identifier_strategy, COALESCE(evidence_observation_id,''), reason, active, created_at, updated_at FROM route_suppressions WHERE job_id = ? AND job_attempt_revision = ? AND institution_profile_id = ? AND institution_profile_revision = ? AND route_revision = ? AND safety_domain_id = ? AND adapter_revision = ? AND identifier_strategy = ? AND active = 1 ORDER BY created_at, id`, key.JobID, key.JobAttemptRevision, key.InstitutionProfileID, key.InstitutionProfileRevision, key.RouteRevision, key.SafetyDomainID, key.AdapterRevision, key.IdentifierStrategy)
   671 	if err != nil {
   672 		return nil, err
   673 	}
   674 	defer func() { _ = rows.Close() }()
   675 	var out []RouteSuppression
   676 	for rows.Next() {
   677 		var s RouteSuppression
   678 		var active int
   679 		if err := rows.Scan(&s.ID, &s.JobID, &s.JobAttemptRevision, &s.InstitutionProfileID, &s.InstitutionProfileRevision, &s.RouteRevision, &s.SafetyDomainID, &s.AdapterRevision, &s.IdentifierStrategy, &s.EvidenceObservationID, &s.Reason, &active, &s.CreatedAt, &s.UpdatedAt); err != nil {
   680 			return nil, err
   681 		}
   682 		s.Active = active != 0
   683 		out = append(out, s)
   684 	}
   685 	return out, rows.Err()
   686 }
   687 
   688 // ArtifactWinner is the insert-only CAS decision for a job attempt. The
   689 // browser holder generation fences the result to the browser claim that
   690 // actually materialized it.
   691 type ArtifactWinner struct {
   692 	JobID                   string `json:"job_id"`
   693 	JobAttemptRevision      int64  `json:"job_attempt_revision"`
   694 	CandidateID             string `json:"candidate_id"`
   695 	BrowserHolderGeneration int64  `json:"browser_holder_generation"`
   696 	SHA256                  string `json:"sha256"`
   697 	CreatedAt               string `json:"created_at"`
   698 }
   699 
```

# Coverage

| Major invariant | Result |
|---|---|
| Phase 0 cutover decision: one transactional payload, closed blocker vocabulary, diagnosable, URL/credential/path/identifier-free, bounded v2→v1 fallback | **Clean — no P0–P2 defect found** |
| Phase 1 profile/candidate/claim authority columns, opaque IDs, tombstones, route suppressions, URL-free durable state | **Clean except where Phase 4 evidence is rebound to a newer revision** |
| Phase 2 explicit candidate-only claim→bind→fresh route→navigate→ack; request correlation, bounded retry, paginated reconciliation, exact rebind, stale-holder abandonment | **Clean — no P0–P2 defect found** |
| Old-peer feature negotiation and legacy explicit fallback | **Clean — no P0–P2 defect found** |
| Protocol strictness and Go/TypeScript/schema disposition gating; transient URL privacy | **Clean — no P0–P2 defect found** |
| Holder-generation fencing for claim/bind/route/navigation callbacks | **Held through navigation acknowledgement; failed for the unowned effect/result/winner path above** |
| Phase 3 indexed keyset traversal, profile/domain rotation, no session mutex during DB scheduling, parked-scaffold bound | **Clean — no P0–P2 defect found** |
| Exact pre-route admission and landed-domain conversion | **Clean — no independent P0–P2 defect found** |
| One global permit across navigation, direct download, provider effect, configured terms, and adoption; takeover waits | **Failed** |
| Artifact-winner CAS and settlement/replay fencing | **Failed** |
| Phase 4 daemon-receipt TTL and signed-out-over-warm / unknown-no-authority precedence | **Clean — no independent P0–P2 defect found** |
| Exact profile revision and holder generation on evidence, including delayed observations and tombstones | **Failed** |
| One authentication-entry owner per authentication claim, typed aggregation, matching closure, later-cycle reopening | **Failed** |
| Terminal/cancel/restart cleanup of local descriptors, claims, bindings, owners, and attention surfaces | **Clean except for the durable resolved-gate reuse defect** |
| Automatic candidate claiming / automatic first route / source-gate bypass / provider readiness / concurrency increase remain disabled | **Clean — no P0–P2 defect found** |

package job

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"papio/internal/store"
)

// CandidateScheduleKey is the stable keyset position of a candidate within a
// safety-domain rotation. CreatedAt and CandidateID are both required: the
// timestamp is not unique, and the opaque ID is the deterministic tie-breaker.
type CandidateScheduleKey struct {
	CreatedAt   string `json:"created_at"`
	CandidateID string `json:"candidate_id"`
}

// CandidateScheduleCursor is daemon-owned continuation state. The cursor is
// deliberately separate from candidate authority: it only remembers traversal
// positions and never makes an otherwise ineligible row claimable. Callers may
// persist the value between polls; an empty cursor starts a new fair rotation.
type CandidateScheduleCursor struct {
	LastGroup          string                          `json:"last_group,omitempty"`
	LastProfileID      string                          `json:"last_profile_id,omitempty"`
	LastGroupByProfile map[string]string               `json:"last_group_by_profile,omitempty"`
	Offsets            map[string]CandidateScheduleKey `json:"offsets,omitempty"`
}

// Encode returns an opaque, URL-safe cursor for a caller that needs to persist
// scheduler continuation. Cursor contents are traversal metadata, not authority
// and may safely be discarded; a discarded cursor merely restarts rotation.
func (c CandidateScheduleCursor) Encode() (string, error) {
	if c.Offsets == nil {
		c.Offsets = map[string]CandidateScheduleKey{}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCandidateScheduleCursor decodes a previously returned cursor. Invalid
// cursors fail closed to an empty rotation rather than becoming an eligibility
// authority or causing a scheduler outage.
func DecodeCandidateScheduleCursor(raw string) CandidateScheduleCursor {
	if strings.TrimSpace(raw) == "" {
		return CandidateScheduleCursor{}
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return CandidateScheduleCursor{}
	}
	var c CandidateScheduleCursor
	if json.Unmarshal(decoded, &c) != nil {
		return CandidateScheduleCursor{}
	}
	if c.Offsets == nil {
		c.Offsets = map[string]CandidateScheduleKey{}
	}
	return c
}

// BrowserCandidateDescriptor is the only object returned by the daemon
// scheduler. It contains no URL, route template, credential, or browser fact;
// the bridge must revalidate these fences before claiming the candidate.
type BrowserCandidateDescriptor struct {
	CandidateID                string
	JobID                      string
	JobAttemptRevision         int64
	InstitutionProfileID       string
	InstitutionProfileRevision int64
	RouteRevision              int64
	RouteClass                 string
	IdentifierStrategy         string
	PreRouteSafetyKey          string
	SafetyDomainID             string
	AdapterRevision            string
	EffectContractID           string
	Status                     string
	CreatedAt                  string
}

// CandidateSchedulePage is one fair scheduler pass. NextCursor is safe to use
// even when the caller claims only some returned descriptors: the unclaimed
// descriptors remain eligible and will be encountered after their domain's
// keyset offset is reached again.
type CandidateSchedulePage struct {
	Candidates []BrowserCandidateDescriptor
	Cursor     CandidateScheduleCursor
	HasMore    bool
}

const candidateSchedulePageSize = 128

// ScheduleEligibleBrowserCandidates returns already-created eligible browser
// candidates in a fair rotation across institution profile plus pre-route/
// provider safety domains. It uses repeated indexed keyset pages, so a large
// backlog is never truncated at an arbitrary first page. Eligibility is read
// from durable state on every pass; no descriptor cache can authorize a claim.
//
// One candidate is returned per safety domain in each rotation pass. A bound,
// route-issued, or navigated scaffold suppresses all siblings in that landed
// domain until the claim settles or is fenced. Current job attempt and exact
// institution-profile revision are checked in SQL, as are active suppressions
// and live claims.
func (js *Store) ScheduleEligibleBrowserCandidates(ctx context.Context, limit int, cursor CandidateScheduleCursor) (CandidateSchedulePage, error) {
	if js == nil || js.S == nil {
		return CandidateSchedulePage{}, errors.New("candidate scheduler requires a store")
	}
	if limit <= 0 {
		limit = 1
	}
	if cursor.LastGroupByProfile == nil {
		cursor.LastGroupByProfile = map[string]string{}
	}
	if cursor.Offsets == nil {
		cursor.Offsets = map[string]CandidateScheduleKey{}
	}

	type row struct {
		descriptor BrowserCandidateDescriptor
		createdAt  string
		group      string
	}
	rows := make([]row, 0)
	var pageCursor CandidateScheduleKey
	for {
		batch, err := js.scheduleEligibleKeysetPage(ctx, pageCursor, candidateSchedulePageSize)
		if err != nil {
			return CandidateSchedulePage{}, err
		}
		for _, c := range batch {
			group := candidateScheduleGroup(c)
			position := CandidateScheduleKey{CreatedAt: c.CreatedAt, CandidateID: c.CandidateID}
			if previous, ok := cursor.Offsets[group]; ok && compareCandidateScheduleKey(position, previous) <= 0 {
				continue
			}
			rows = append(rows, row{descriptor: c, createdAt: c.CreatedAt, group: group})
		}
		if len(batch) < candidateSchedulePageSize {
			break
		}
		last := batch[len(batch)-1]
		pageCursor = CandidateScheduleKey{CreatedAt: last.CreatedAt, CandidateID: last.CandidateID}
	}

	groups := make(map[string][]row)
	profileGroups := make(map[string][]string)
	for _, candidate := range rows {
		if _, ok := groups[candidate.group]; !ok {
			profile := candidate.descriptor.InstitutionProfileID
			profileGroups[profile] = append(profileGroups[profile], candidate.group)
		}
		groups[candidate.group] = append(groups[candidate.group], candidate)
	}
	profiles := make([]string, 0, len(profileGroups))
	for profile := range profileGroups {
		profiles = append(profiles, profile)
		sort.Strings(profileGroups[profile])
	}
	sort.Strings(profiles)
	if len(profiles) == 0 {
		return CandidateSchedulePage{Cursor: cursor}, nil
	}

	start := 0
	if cursor.LastProfileID != "" {
		for i, profile := range profiles {
			if profile > cursor.LastProfileID {
				start = i
				break
			}
			start = (i + 1) % len(profiles)
		}
	}
	out := make([]BrowserCandidateDescriptor, 0, limit)
	next := CandidateScheduleCursor{
		LastGroup: cursor.LastGroup, LastProfileID: cursor.LastProfileID,
		LastGroupByProfile: make(map[string]string, len(cursor.LastGroupByProfile)+len(profiles)),
		Offsets:            make(map[string]CandidateScheduleKey, len(cursor.Offsets)+limit),
	}
	for profile, group := range cursor.LastGroupByProfile {
		next.LastGroupByProfile[profile] = group
	}
	for group, position := range cursor.Offsets {
		next.Offsets[group] = position
	}
	// Profile is the outer rotation and the pre-route/provider domain is the
	// inner rotation. This prevents one institution with a large domain set
	// from starving every other institution.
	maxSteps := len(rows) + len(profiles)
	selectedGroups := make(map[string]bool, len(profileGroups))
	for step := range maxSteps {
		if len(out) >= limit {
			break
		}
		profile := profiles[(start+step)%len(profiles)]
		profileDomainGroups := profileGroups[profile]
		groupStart := 0
		if previous := next.LastGroupByProfile[profile]; previous != "" {
			for i, group := range profileDomainGroups {
				if group > previous {
					groupStart = i
					break
				}
				groupStart = (i + 1) % len(profileDomainGroups)
			}
		}
		group := profileDomainGroups[groupStart]
		if selectedGroups[group] {
			continue
		}
		selectedGroups[group] = true
		members := groups[group]
		position, hasPosition := next.Offsets[group]
		var selected row
		found := false
		for _, candidate := range members {
			key := CandidateScheduleKey{CreatedAt: candidate.createdAt, CandidateID: candidate.descriptor.CandidateID}
			if !hasPosition || compareCandidateScheduleKey(key, position) > 0 {
				selected = candidate
				found = true
				break
			}
		}
		if !found {
			continue
		}
		position = CandidateScheduleKey{CreatedAt: selected.createdAt, CandidateID: selected.descriptor.CandidateID}
		out = append(out, selected.descriptor)
		next.Offsets[group] = position
		next.LastGroupByProfile[profile] = group
		next.LastProfileID = profile
		next.LastGroup = group
	}
	// There may be eligible groups beyond this page. A wrapped rotation also
	// reports more work when a group has another keyset row after its selected
	// position, allowing callers to continue past positions 200 and 500.
	hasMore := false
	for group, members := range groups {
		position, selected := next.Offsets[group]
		for _, candidate := range members {
			key := CandidateScheduleKey{CreatedAt: candidate.createdAt, CandidateID: candidate.descriptor.CandidateID}
			if !selected || compareCandidateScheduleKey(key, position) > 0 {
				hasMore = true
				break
			}
		}
		if hasMore {
			break
		}
	}
	if len(out) == 0 {
		next = cursor
	}
	return CandidateSchedulePage{Candidates: out, Cursor: next, HasMore: hasMore}, nil
}

func candidateScheduleGroup(c BrowserCandidateDescriptor) string {
	// Landed safety domains are global provider fences. They therefore own one
	// parked scaffold even when two explicit candidates arrived through
	// different pre-route keys. The profile/pre-route tuple is only a fallback
	// for descriptors that have not yet been assigned a landed domain.
	if strings.TrimSpace(c.SafetyDomainID) != "" {
		return "domain\x00" + c.SafetyDomainID
	}
	return "profile\x00" + c.InstitutionProfileID + "\x00" + c.PreRouteSafetyKey
}

func compareCandidateScheduleKey(a, b CandidateScheduleKey) int {
	if a.CreatedAt < b.CreatedAt {
		return -1
	}
	if a.CreatedAt > b.CreatedAt {
		return 1
	}
	if a.CandidateID < b.CandidateID {
		return -1
	}
	if a.CandidateID > b.CandidateID {
		return 1
	}
	return 0
}

func (js *Store) scheduleEligibleKeysetPage(ctx context.Context, after CandidateScheduleKey, limit int) ([]BrowserCandidateDescriptor, error) {
	now := store.Now()
	where := `
		WHERE c.status='eligible'
		  AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM materialization_claims live
			 WHERE live.candidate_id=c.id
			   AND live.phase IN ('claimed','bound','route_issued','navigated')
			   AND (live.lease_until IS NULL OR live.lease_until > ?)
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM route_suppressions suppression
			 WHERE suppression.active=1
			   AND suppression.job_id=c.job_id
			   AND suppression.job_attempt_revision=c.job_attempt_revision
			   AND suppression.institution_profile_id=c.institution_profile_id
			   AND suppression.institution_profile_revision=c.institution_profile_revision
			   AND suppression.route_revision=c.route_revision
			   AND suppression.safety_domain_id=c.safety_domain_id
			   AND suppression.adapter_revision=c.adapter_revision
			   AND suppression.identifier_strategy=c.identifier_strategy
		  )
		  AND NOT EXISTS (
			SELECT 1
			  FROM materialization_claims parked
			  JOIN browser_candidates sibling ON sibling.id=parked.candidate_id
			 WHERE sibling.safety_domain_id=c.safety_domain_id
			   AND parked.phase IN ('bound','route_issued','navigated')
			   AND (parked.lease_until IS NULL OR parked.lease_until > ?)
		  )`
	args := []any{now, now}
	if after.CreatedAt != "" || after.CandidateID != "" {
		where += ` AND (c.created_at > ? OR (c.created_at = ? AND c.id > ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.CandidateID)
	}
	query := `SELECT c.id, c.job_id, c.job_attempt_revision, c.institution_profile_id,
		c.institution_profile_revision, c.route_revision, c.route_class,
		c.identifier_strategy, c.pre_route_safety_key, c.safety_domain_id,
		c.adapter_revision, c.effect_contract_id, c.status, c.created_at
		FROM browser_candidates c
		JOIN institution_profiles p ON p.id=c.institution_profile_id ` + where + `
		ORDER BY c.created_at, c.id LIMIT ?`
	args = append(args, limit)
	rows, err := js.S.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BrowserCandidateDescriptor, 0, limit)
	for rows.Next() {
		var c BrowserCandidateDescriptor
		if err := rows.Scan(&c.CandidateID, &c.JobID, &c.JobAttemptRevision,
			&c.InstitutionProfileID, &c.InstitutionProfileRevision, &c.RouteRevision,
			&c.RouteClass, &c.IdentifierStrategy, &c.PreRouteSafetyKey,
			&c.SafetyDomainID, &c.AdapterRevision, &c.EffectContractID,
			&c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

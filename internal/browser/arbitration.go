// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// browserSession is one native-host connection that said hello.
type browserSession struct {
	ID               string
	ExtensionVersion string
	AdapterVersions  map[string]string
	Features         []string
	HelloAt          time.Time
	LastSyncAt       time.Time
	Outdated         bool

	adapterUpgradeRepairPending bool
	needsAck                    bool
	demotedNotice               bool
	pendingDevReload            string
}

// sessionArbitration owns the browser holder state machine. Bridge serializes
// calls with its mutex, but only these transition methods mutate this state.
type sessionArbitration struct {
	holder        *browserSession
	pending       map[string]*browserSession
	deniedHellos  int
	takeovers     int
	epoch         int64
	reservedFor   string
	reservedUntil time.Time
	nextEpoch     func() (int64, error)
}

type arbitrationTransition struct {
	changed             bool
	previous            *browserSession
	generationAttempted bool
	generationErr       error
}

type helloDecision struct {
	role              string
	session           *browserSession
	transition        arbitrationTransition
	releasedForReload bool
}

type pollDecision struct {
	known      bool
	holder     bool
	transition arbitrationTransition
}

func newSessionArbitration(nextEpoch func() (int64, error)) sessionArbitration {
	return sessionArbitration{
		pending:   make(map[string]*browserSession),
		nextEpoch: nextEpoch,
	}
}

func (a *sessionArbitration) generation() int64 { return a.epoch }

func (a *sessionArbitration) holderSession() *browserSession { return a.holder }

func (a *sessionArbitration) session(sessionID string) *browserSession {
	if a.holder != nil && a.holder.ID == sessionID {
		return a.holder
	}
	return a.pending[sessionID]
}

func (a *sessionArbitration) known(sessionID string) bool { return a.session(sessionID) != nil }

func (a *sessionArbitration) isHolder(sessionID string) bool {
	return a.holder != nil && a.holder.ID == sessionID
}
func (a *sessionArbitration) holderMatches(sessionID string, generation int64) bool {
	return a.epoch == generation && a.isHolder(sessionID)
}

func (a *sessionArbitration) holderVersion() string {
	if a.holder == nil {
		return ""
	}
	return a.holder.ExtensionVersion
}

func (a *sessionArbitration) hello(session *browserSession, now time.Time, refreshGeneration bool) helloDecision {
	holderAlive := a.holder != nil && now.Sub(a.holder.LastSyncAt) <= sessionStaleAfter
	sameSession := a.isHolder(session.ID)
	legacyInvolved := session.ID == legacySessionID || (a.holder != nil && a.holder.ID == legacySessionID)
	reloadDeparture := a.holder != nil &&
		a.reservedFor == a.holder.ID &&
		now.Before(a.reservedUntil) &&
		!sameSession &&
		!legacyInvolved
	if a.holder != nil && holderAlive && !sameSession && !legacyInvolved && !reloadDeparture {
		a.pending[session.ID] = session
		a.deniedHellos++
		return helloDecision{role: sessionRolePending, session: session}
	}

	previous := a.holder
	if reloadDeparture {
		_, _ = a.release(previous.ID)
	}
	changed := previous == nil || previous.ID != session.ID
	if previous != nil && !sameSession && !reloadDeparture {
		a.takeovers++
	}
	delete(a.pending, session.ID)
	transition := arbitrationTransition{changed: changed, previous: previous}
	if (changed || refreshGeneration) && a.nextEpoch != nil {
		transition.generationAttempted = true
		generation, err := a.nextEpoch()
		if err != nil {
			transition.generationErr = err
		} else {
			a.epoch = generation
		}
	}
	a.clearReloadReservation()
	a.holder = session
	return helloDecision{
		role:              sessionRoleHolder,
		session:           session,
		transition:        transition,
		releasedForReload: reloadDeparture,
	}
}

func (a *sessionArbitration) poll(sessionID string, now time.Time) pollDecision {
	a.prune(now)
	if a.isHolder(sessionID) {
		a.holder.LastSyncAt = now
		return pollDecision{known: true, holder: true}
	}
	session := a.pending[sessionID]
	if session == nil {
		return pollDecision{}
	}
	session.LastSyncAt = now
	if a.holder == nil {
		if a.reloadReserved(now) {
			return pollDecision{known: true}
		}
		transition := a.promote(session, false)
		return pollDecision{known: true, holder: true, transition: transition}
	}
	if now.Sub(a.holder.LastSyncAt) > sessionStaleAfter {
		transition := a.promote(session, true)
		return pollDecision{known: true, holder: true, transition: transition}
	}
	return pollDecision{known: true}
}

func (a *sessionArbitration) claim(prefix string) (string, arbitrationTransition, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", arbitrationTransition{}, errors.New("browser session id is required")
	}
	var matches []*browserSession
	if a.holder != nil && strings.HasPrefix(a.holder.ID, prefix) {
		matches = append(matches, a.holder)
	}
	for id, session := range a.pending {
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, session)
		}
	}
	switch {
	case len(matches) == 0:
		return "", arbitrationTransition{}, fmt.Errorf("unknown browser session %q (run 'papio browser sessions')", prefix)
	case len(matches) > 1:
		return "", arbitrationTransition{}, fmt.Errorf("browser session prefix %q is ambiguous (run 'papio browser sessions')", prefix)
	case matches[0] == a.holder:
		return a.holder.ID, arbitrationTransition{}, nil
	}
	transition := a.promote(matches[0], false)
	return a.holder.ID, transition, nil
}

func (a *sessionArbitration) promote(session *browserSession, dropPrevious bool) arbitrationTransition {
	previous := a.holder
	if previous != nil && previous.ID != session.ID {
		previous.demotedNotice = !dropPrevious
		previous.pendingDevReload = ""
		if !dropPrevious {
			a.pending[previous.ID] = previous
		} else {
			delete(a.pending, previous.ID)
		}
	}
	transition := arbitrationTransition{changed: previous == nil || previous.ID != session.ID, previous: previous}
	if a.nextEpoch != nil {
		transition.generationAttempted = true
		generation, err := a.nextEpoch()
		if err != nil {
			transition.generationErr = err
		} else {
			a.epoch = generation
		}
	}
	delete(a.pending, session.ID)
	session.needsAck = true
	a.clearReloadReservation()
	session.adapterUpgradeRepairPending = true
	a.holder = session
	a.takeovers++
	return transition
}

func (a *sessionArbitration) release(sessionID string) (*browserSession, bool) {
	delete(a.pending, sessionID)
	if !a.isHolder(sessionID) {
		return nil, false
	}
	departed := a.holder
	a.holder = nil
	a.epoch = 0
	return departed, true
}

func (a *sessionArbitration) consumeHolderAck(sessionID string) *browserSession {
	if !a.isHolder(sessionID) || !a.holder.needsAck {
		return nil
	}
	a.holder.needsAck = false
	return a.holder
}

func (a *sessionArbitration) consumeDemotedNotice(sessionID string) bool {
	session := a.pending[sessionID]
	if session == nil || !session.demotedNotice {
		return false
	}
	session.demotedNotice = false
	return true
}

func (a *sessionArbitration) consumeAdapterUpgradeRepair(now time.Time) *browserSession {
	if a.holder == nil ||
		a.holder.Outdated ||
		!a.holder.adapterUpgradeRepairPending ||
		len(a.holder.AdapterVersions) == 0 ||
		now.Sub(a.holder.LastSyncAt) > sessionStaleAfter {
		return nil
	}
	a.holder.adapterUpgradeRepairPending = false
	return a.holder
}

func (a *sessionArbitration) requestDevReload(reloadID string) (string, string, error) {
	if a.holder == nil {
		return "", "", errors.New("no browser session holds the bridge")
	}
	if a.holder.pendingDevReload != "" {
		return a.holder.ID, a.holder.pendingDevReload, nil
	}
	a.holder.pendingDevReload = reloadID
	return a.holder.ID, reloadID, nil
}

func (a *sessionArbitration) pendingDevReload(sessionID string) string {
	if !a.isHolder(sessionID) {
		return ""
	}
	return a.holder.pendingDevReload
}

func (a *sessionArbitration) emitDevReload(sessionID, reloadID string, now time.Time) bool {
	if !a.isHolder(sessionID) || reloadID == "" || a.holder.pendingDevReload != reloadID {
		return false
	}
	a.holder.pendingDevReload = ""
	a.reservedFor = sessionID
	a.reservedUntil = now.Add(devReloadReservation)
	return true
}

func (a *sessionArbitration) reloadReserved(now time.Time) bool {
	return a.reservedFor != "" && now.Before(a.reservedUntil)
}

func (a *sessionArbitration) clearReloadReservation() {
	a.reservedFor = ""
	a.reservedUntil = time.Time{}
}

func (a *sessionArbitration) prune(now time.Time) {
	for id, session := range a.pending {
		if now.Sub(session.LastSyncAt) > pendingExpireAfter {
			delete(a.pending, id)
		}
	}
}

func (a *sessionArbitration) summaries() ([]SessionSummary, int, int) {
	sessions := make([]SessionSummary, 0, len(a.pending)+1)
	if a.holder != nil {
		sessions = append(sessions, summarize(a.holder, true))
	}
	rest := make([]*browserSession, 0, len(a.pending))
	for _, session := range a.pending {
		rest = append(rest, session)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].LastSyncAt.After(rest[j].LastSyncAt) })
	for _, session := range rest {
		sessions = append(sessions, summarize(session, false))
	}
	return sessions, a.deniedHellos, a.takeovers
}

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import "time"

func (a *sessionArbitration) setHolderForTest(session *browserSession, generation int64) {
	a.holder = session
	a.epoch = generation
}

func (a *sessionArbitration) advanceGenerationForTest() {
	a.epoch++
}
func (a *sessionArbitration) setGenerationForTest(generation int64) {
	a.epoch = generation
}
func (a *sessionArbitration) retreatGenerationForTest() {
	a.epoch--
}

func (a *sessionArbitration) setHolderOnlyForTest(session *browserSession) {
	a.holder = session
}

func (a *sessionArbitration) setHolderLastSyncForTest(at time.Time) {
	if a.holder != nil {
		a.holder.LastSyncAt = at
	}
}

func (a *sessionArbitration) setHolderAdapterVersionForTest(adapter, version string) {
	if a.holder != nil {
		a.holder.AdapterVersions[adapter] = version
	}
}

func promoteForTest(b *Bridge, session *browserSession, reason string) {
	transition := b.arbitration.promote(session, false)
	b.applyPromotion(transition, reason)
}

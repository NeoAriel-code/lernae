package acquisition

import "time"

const maxContentVerificationTimeout = 10 * time.Minute

// Only the three content-verifying capability calls opt in. Default retains
// each service's existing timeout (including focused tests' shorter deadlines).
func contentVerificationPolicy(capability any, fallback time.Duration) ContentVerificationPolicy {
	defaults := ContentVerificationPolicy{Discovery: fallback, Resolution: fallback, Dispatch: fallback}
	provider, ok := capability.(ContentVerificationTimeouts)
	if !ok {
		return defaults
	}
	policy := provider.ContentVerificationTimeouts()
	for _, budget := range []time.Duration{policy.Discovery, policy.Resolution, policy.Dispatch} {
		if budget != 0 && (budget < providerTimeout || budget > maxContentVerificationTimeout) {
			return defaults
		}
	}
	if policy.Discovery == 0 {
		policy.Discovery = fallback
	}
	if policy.Resolution == 0 {
		policy.Resolution = fallback
	}
	if policy.Dispatch == 0 {
		policy.Dispatch = fallback
	}
	return policy
}

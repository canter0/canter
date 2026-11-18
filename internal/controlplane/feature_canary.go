package controlplane

// SetFeatureCanaryWorkspace scopes the five rollout flags at startup. An empty
// value retains the global behavior of each flag. A missing workspace matches
// no requests, including after a canary workspace has been deleted.
func (s *Store) SetFeatureCanaryWorkspace(workspace string) {
	s.featureCanaryWorkspace = workspace
}

func (s *Store) inFeatureCanary(workspace string) bool {
	return s.featureCanaryWorkspace == "" || s.featureCanaryWorkspace == workspace
}

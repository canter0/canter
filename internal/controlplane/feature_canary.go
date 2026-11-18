package controlplane

// SetFeatureCanaryWorkspace enables the five canary features for one workspace
// at startup. Keep the global switches off during a canary so a rollback to
// a binary without workspace scoping cannot activate them globally.
func (s *Store) SetFeatureCanaryWorkspace(workspace string) {
	s.featureCanaryWorkspace = workspace
}

func (s *Store) canaryFeatureEnabled(workspace string, global bool) bool {
	if s.featureCanaryWorkspace == "" {
		return global
	}
	return s.featureCanaryWorkspace == workspace
}

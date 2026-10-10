package service

// ConfiguredRoot is the data root the service records into. Until set-root
// exists (a later commit) it is always the control directory.
func ConfiguredRoot(ctrl Control) (string, error) { return ctrl.Dir, nil }

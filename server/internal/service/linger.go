package service

// Linger (issue #3) is added with the lifecycle commit; until then the
// service runs until it is stopped.

func (s *Service) clientsChanged(n int) {}
func (s *Service) stopLinger()          {}
func (s *Service) lingerText() string   { return "" }

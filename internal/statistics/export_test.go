package statistics

// StateForTest lets the external SQLite integration tests inspect bucket invariants.
func (s *Engine) StateForTest() *Accumulator { return &s.state }

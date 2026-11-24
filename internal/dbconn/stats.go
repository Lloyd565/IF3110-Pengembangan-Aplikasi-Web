package dbconn

type PoolSnapshot struct {
	DBMS        string
	Active      float64
	Idle        float64
	WaitSeconds float64
	WaitCount   float64
}

func (r *Registry) Stats() []PoolSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := []PoolSnapshot{}
	for _, t := range r.pools {
		p := PoolSnapshot{DBMS: string(t.DBMS)}
		if t.Postgres != nil {
			s := t.Postgres.Stat()
			p.Active = float64(s.AcquiredConns())
			p.Idle = float64(s.IdleConns())
			p.WaitSeconds = s.EmptyAcquireWaitTime().Seconds()
			p.WaitCount = float64(s.EmptyAcquireCount())
		}
		if t.Maria != nil {
			s := t.Maria.Stats()
			p.Active = float64(s.InUse)
			p.Idle = float64(s.Idle)
			p.WaitSeconds = s.WaitDuration.Seconds()
			p.WaitCount = float64(s.WaitCount)
		}
		result = append(result, p)
	}
	return result
}

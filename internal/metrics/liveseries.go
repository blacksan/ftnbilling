package metrics

import "time"

// RecentWAN returns the WAN port's last n five-minute buckets (oldest first,
// the bucket holding now included) as start times and bit/s. A bucket with no
// sample is nil. "down" is the WAN's rx (the internet to the customers).
func (c *Collector) RecentWAN(now time.Time, n int) (t []time.Time, down, up []*int64) {
	if n <= 0 {
		return nil, nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	end := BucketStart(T5, now) + T5.Seconds()
	from := time.Unix(end-int64(n)*T5.Seconds(), 0)
	pts := fillBuckets(c.D.Ports[c.cfg.WAN], T5, from, time.Unix(end, 0))
	for _, p := range pts {
		t = append(t, p.T)
		if p.C.N == 0 {
			down, up = append(down, nil), append(up, nil)
			continue
		}
		secs := float64(T5.Seconds())
		d, u := int64(float64(p.C.Down)*8/secs), int64(float64(p.C.Up)*8/secs)
		down, up = append(down, &d), append(up, &u)
	}
	return t, down, up
}

// FlatUsage is the bytes (both directions) a flat moved today and this month
// (EAT days), from the daily series.
func (c *Collector) FlatUsage(code string, now time.Time) (today, month uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.D.Accts[code]
	if s == nil {
		return 0, 0
	}
	nowE := now.In(EAT)
	dayStart := time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, EAT)
	monthStart := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, EAT)
	end := now.Add(time.Second)
	t := s.Sum(TD, dayStart, end)
	m := s.Sum(TD, monthStart, end)
	return t.Down + t.Up, m.Down + m.Up
}

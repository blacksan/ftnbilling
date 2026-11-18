package metrics

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

func evening(t time.Time) float64 {
	e := t.In(EAT)
	h := float64(e.Hour()) + float64(e.Minute())/60
	return 0.15 + 0.85*math.Exp(-math.Pow((h-20.5)/3.2, 2))
}

// SeedDemo fills the series with plausible synthetic history for the current
// accounts (ROUTER_MOCK demos only). It adds to whatever exists.
func (c *Collector) SeedDemo(now time.Time, days int) {
	accts := c.acctMap()
	c.mu.Lock()
	defer c.mu.Unlock()
	rng := rand.New(rand.NewSource(42))
	type prof struct {
		cap, base float64
		heavy     bool
	}
	profs := map[string]prof{}
	codes := make([]string, 0, len(accts))
	for code := range accts {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for i, code := range codes {
		a := accts[code]
		cp := float64(a.SpeedMbps)
		if cp <= 0 {
			cp = 10
		}
		profs[code] = prof{cap: cp * 1e6, base: 0.02 + 0.2*rng.Float64(), heavy: i%4 == 1}
	}
	start := time.Unix(BucketStart(T5, now.Add(-time.Duration(days)*24*time.Hour)), 0)
	for t := start; t.Before(now); t = t.Add(5 * time.Minute) {
		ev := evening(t)
		var sumDown, sumUp float64
		for code, p := range profs {
			rate := p.cap * p.base * ev * (0.4 + 1.2*rng.Float64())
			if p.heavy && ev > 0.5 && rng.Float64() < 0.6 {
				rate = p.cap * (0.92 + 0.08*rng.Float64())
			}
			if rate > p.cap {
				rate = p.cap
			}
			up := rate * (0.05 + 0.1*rng.Float64())
			down := uint64(rate * 300 / 8)
			upB := uint64(up * 300 / 8)
			sumDown += rate
			sumUp += up
			atCap := rate >= CapFraction*p.cap
			pk := rate * (1 + 0.15*rng.Float64())
			if pk > p.cap {
				pk = p.cap
			}
			c.getAcct(code).Add(t, func(cell *Cell) {
				cell.Down += down
				cell.Up += upB
				cell.PkDown = maxU(cell.PkDown, uint64(pk))
				cell.PkUp = maxU(cell.PkUp, uint64(up*1.2))
				cell.OnMin += 5
				if atCap {
					cell.CapMin += 5
				}
				cell.N++
			})
			c.syntheticServices(t, code, down+upB)
		}
		wanRx, wanTx := sumDown*1.04, sumUp*1.04
		dRx, dTx := uint64(wanRx*300/8), uint64(wanTx*300/8)
		pkRx, pkTx := uint64(wanRx*1.25), uint64(wanTx*1.25)
		c.getPort(c.cfg.WAN).Add(t, func(cell *Cell) {
			cell.Down += dRx
			cell.Up += dTx
			cell.PkDown = maxU(cell.PkDown, pkRx)
			cell.PkUp = maxU(cell.PkUp, pkTx)
			cell.N++
		})
		oRx, oTx := uint64(sumUp*300/8), uint64(sumDown*300/8)
		c.getPort(c.cfg.OLT).Add(t, func(cell *Cell) {
			cell.Down += oRx
			cell.Up += oTx
			cell.PkDown = maxU(cell.PkDown, uint64(sumUp*1.25))
			cell.PkUp = maxU(cell.PkUp, uint64(sumDown*1.25))
			cell.N++
		})
		mk := MonthKey(t)
		m := c.D.Months[mk]
		if m == nil {
			m = &MonthUse{}
			c.D.Months[mk] = m
		}
		m.Rx += dRx
		m.Tx += dTx
		for i, target := range c.cfg.PingTargets {
			rtt := 9 + 3*float64(i) + 6*rng.Float64() + 22*ev*rng.Float64()
			loss := 0.0
			if rng.Float64() < 0.04 {
				loss = 20
			}
			getSeries(c.D.Ping, target).Add(t, func(cell *Cell) {
				cell.N++
				cell.RTT += rtt
				cell.RTTN++
				cell.RTTMax = math.Max(cell.RTTMax, rtt)
				cell.Loss += loss
			})
		}
		c.D.Sys.Add(t, func(cell *Cell) {
			cell.N++
			cell.CPU += 4 + 30*ev*rng.Float64()
			cell.Temp += 42 + 9*ev + 2*rng.Float64()
			cell.TempN++
		})
	}
	c.D.Prune(now)
}

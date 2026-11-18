package routeros

import (
	"context"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// Synthetic telemetry so the metrics page can be demoed with ROUTER_MOCK=true.
// Counters grow with wall-clock time at rates that follow an evening peak.

type mockSess struct {
	in, out uint64
	started time.Time
}

type mockState struct {
	sess   map[string]*mockSess
	ifaces map[string]*[2]uint64
	last   time.Time // last Interfaces() call
	lastS  time.Time // last ActiveSessions() call
	rng    *rand.Rand
}

func (m *Mock) tel() *mockState {
	if m.ms == nil {
		m.ms = &mockState{sess: map[string]*mockSess{}, ifaces: map[string]*[2]uint64{}, rng: rand.New(rand.NewSource(7)), last: time.Now(), lastS: time.Now()}
	}
	return m.ms
}

// eveningFactor is 0.2 at night up to 1.0 around 20:00 EAT.
func eveningFactor(t time.Time) float64 {
	h := float64(t.In(time.FixedZone("EAT", 3*3600)).Hour()) + float64(t.Minute())/60
	return 0.2 + 0.8*math.Exp(-math.Pow((h-20)/3.5, 2))
}

// rateCapBps parses the profile rate-limit ("10M/10M") of a user.
func (m *Mock) capBps(user string) float64 {
	u := m.users[user]
	rl := m.profiles[u.Profile].RateLimit
	a, _, _ := strings.Cut(rl, "/")
	a = strings.TrimSuffix(a, "M")
	n, _ := strconv.ParseFloat(a, 64)
	if n <= 0 {
		n = 10
	}
	return n * 1e6
}

func (m *Mock) ActiveSessions(context.Context) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	st := m.tel()
	now := time.Now()
	dt := now.Sub(st.lastS).Seconds()
	st.lastS = now
	var out []Session
	for user, ip := range m.active {
		s := st.sess[user]
		if s == nil {
			s = &mockSess{started: now}
			st.sess[user] = s
		}
		cap := m.capBps(user)
		down := cap * (0.1 + 0.8*st.rng.Float64()) * eveningFactor(now)
		s.out += uint64(down / 8 * dt)
		s.in += uint64(down / 8 * dt * 0.08)
		out = append(out, Session{ID: "*" + user, User: user, Address: ip, MAC: m.users[user].MAC,
			BytesIn: s.in, BytesOut: s.out, Uptime: now.Sub(s.started)})
	}
	for u := range st.sess {
		if _, on := m.active[u]; !on {
			delete(st.sess, u)
		}
	}
	return out, nil
}

func (m *Mock) SimpleQueues(context.Context) ([]SimpleQueue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []SimpleQueue
	for user := range m.active {
		cap := int64(m.capBps(user))
		out = append(out, SimpleQueue{Name: "<hotspot-" + user + ">", Target: m.active[user] + "/32", MaxA: cap, MaxB: cap,
			RateA: cap / 20, RateB: int64(float64(cap) * (0.2 + 0.75*m.tel().rng.Float64()))})
	}
	return out, nil
}

func (m *Mock) Interfaces(context.Context) ([]Iface, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	st := m.tel()
	now := time.Now()
	dt := now.Sub(st.last).Seconds()
	st.last = now
	ev := eveningFactor(now)
	// {name, rx share, tx share}: WAN rx = download from the internet.
	spec := []struct {
		name           string
		rxMbps, txMbps float64
	}{
		{"ether1", 300, 40}, {"sfp-sfpplus1", 40, 300}, {"vlan100-customers", 40, 300}, {"br-hotspot", 40, 300}, {"ether5", 0, 0},
	}
	var out []Iface
	for _, sp := range spec {
		c := st.ifaces[sp.name]
		if c == nil {
			c = &[2]uint64{}
			st.ifaces[sp.name] = c
		}
		j := 0.85 + 0.3*st.rng.Float64()
		c[0] += uint64(sp.rxMbps * 1e6 * ev * j / 8 * dt)
		c[1] += uint64(sp.txMbps * 1e6 * ev * j / 8 * dt)
		out = append(out, Iface{Name: sp.name, Type: "ether", RxBytes: c[0], TxBytes: c[1], Running: true})
	}
	out = append(out, Iface{Name: "veth-ispbill", Type: "veth", Running: true, Dynamic: true})
	return out, nil
}

func (m *Mock) DNSCache(context.Context) ([]DNSEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	return []DNSEntry{
		{Name: "rr1---sn-abc.googlevideo.com", Data: "142.250.0.10", Type: "A"},
		{Name: "api.whatsapp.net", Data: "157.240.0.60", Type: "A"},
	}, nil
}

func (m *Mock) PingHost(_ context.Context, target string, count int) (PingResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return PingResult{}, m.fail
	}
	st := m.tel()
	rtt := 9 + 8*st.rng.Float64() + 25*eveningFactor(time.Now())*st.rng.Float64()
	lost := 0
	if st.rng.Float64() < 0.05 {
		lost = 1
	}
	return PingResult{Target: target, Sent: count, Received: count - lost,
		AvgRTT: time.Duration(rtt * float64(time.Millisecond)), LossPct: 100 * float64(lost) / float64(count)}, nil
}

func (m *Mock) System(context.Context) (SysInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return SysInfo{}, m.fail
	}
	st := m.tel()
	return SysInfo{CPULoad: 5 + int(30*eveningFactor(time.Now())*st.rng.Float64()), FreeMem: 700 << 20, TotalMem: 1 << 30,
		Uptime: "3d4h", TempC: 44 + 8*st.rng.Float64(), HasTemp: true}, nil
}

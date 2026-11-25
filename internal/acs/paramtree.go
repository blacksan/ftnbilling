package acs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Parameter-tree discovery. After a trusted ONU has reported (its Inform, its tasks and the live reads are done), the
// ACS walks the data model of the box in the same CWMP session: GetParameterNames with NextLevel, breadth first, from
// the root the Inform showed (InternetGatewayDevice. or Device.), then GetParameterValues in chunks for the values
// worth keeping. The result is stored as the ONU's current tree (store.ONUTree) and the resolver (resolver.go) finds
// the real vendor paths in it; the metric collection reads those instead of guessing.
//
// A walk is bounded in every way: requests (maxWalkGPN + maxWalkGPV, on top of the session's own allowance),
// parameters stored, instances read per table, depth and wall-clock time, and at most maxConcurrentWalks run at once
// (a box that finds the slots taken is walked at its next check-in). Values of secret parameters are never asked for.
const (
	maxWalkGPN         = 400 // GetParameterNames requests in one walk
	maxWalkGPV         = 24  // GetParameterValues requests in one walk
	walkChunk          = 40  // names per GetParameterValues
	walkInstances      = 8   // instances read of one table (Host.1 ... Host.8)
	walkDepth          = 14
	walkBudget         = 150 * time.Second
	walkRetry          = time.Hour // a walk that failed is not tried again sooner
	maxConcurrentWalks = 3
	maxNamesPerReply   = 1500 // entries of one GetParameterNamesResponse that are looked at
	maxLeavesPerObject = 400  // parameters kept of one object (a wide object cannot use up the room of the rest)
)

// Walk reasons (ONUTree.Reason).
const (
	WalkFirst   = "first"   // no tree yet
	WalkMarked  = "marked"  // the retroactive pass marked a box that was already reporting
	WalkVersion = "version" // the software or hardware version changed
	WalkReset   = "reset"   // a factory reset (0 BOOTSTRAP, or a changed ParameterKey)
)

// walkReason says whether the ONU is to be walked now, and why ("" = not). It is pure.
func walkReason(o store.ONU, t store.ONUTree, have bool, reset bool, now time.Time) string {
	if o.Root == "" {
		return ""
	}
	switch {
	case have && reset:
		return WalkReset
	case have && (t.SWVersion != o.SWVersion || t.HWVersion != o.HWVersion || t.Root != o.Root):
		return WalkVersion
	}
	retryLater := !o.State.WalkTry.IsZero() && now.Sub(o.State.WalkTry) < walkRetry
	switch {
	case !have && reset:
		return WalkReset
	case !have && retryLater:
		return ""
	case !have && o.State.WalkWanted:
		return WalkMarked
	case !have:
		return WalkFirst
	case o.State.WalkWanted && !retryLater:
		return WalkMarked
	}
	return ""
}

// maybeWalk walks the tree of the ONU when it is due and a slot is free. It never fails the session.
func (s *Service) maybeWalk(ctx context.Context, ss *session, onuID string, reset bool) {
	var o store.ONU
	var t store.ONUTree
	var have bool
	s.st.View(func(d *store.Data) {
		var ok bool
		if o, ok = d.ONUs[onuID]; ok {
			o = store.CloneONU(o)
			t, have = d.ONUTreeOf(onuID)
			t = t.Clone()
		}
	})
	reason := walkReason(o, t, have, reset, s.now())
	if reason == "" {
		return
	}
	select {
	case s.walkSlots <- struct{}{}:
		defer func() { <-s.walkSlots }()
	default:
		s.log.Info("ACS: parameter tree walk waits for a free slot", "onu", onuID)
		return
	}
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		cur.State.WalkTry = now
		d.ONUs[onuID] = cur
		return nil
	})
	ss.allow(maxWalkGPN + maxWalkGPV + 8)
	params, truncated, err := s.walkTree(ctx, ss, o)
	if err != nil || len(params) == 0 {
		if ctx.Err() == nil {
			msg := "no parameters listed"
			if err != nil {
				msg = err.Error()
			}
			s.log.Info("ACS: parameter tree not walked", "onu", onuID, "reason", reason, "err", msg)
		}
		return
	}
	text, n := store.EncodeTree(params)
	stored := store.ONUTree{ID: onuID, WalkedAt: s.now(), Root: o.Root, SWVersion: o.SWVersion, HWVersion: o.HWVersion, Reason: reason, Count: n,
		Truncated: truncated || len(params) > n, Text: text}
	stored.Resolved = Resolve(o.Root, stored.Params())
	_ = s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		d.ONUTrees[onuID] = stored
		cur.State.WalkWanted = false
		d.ONUs[onuID] = cur
		return nil
	})
	s.log.Info("ACS: parameter tree walked", "onu", onuID, "reason", reason, "params", n, "truncated", stored.Truncated, "gaps", len(stored.Resolved.Gaps))
}

// walkTree lists the parameters of the box and reads the values of the ones worth it. A walk that the session cut
// short returns what it had with truncated set; an error is returned only when nothing could be listed.
func (s *Service) walkTree(ctx context.Context, ss *session, o store.ONU) (params []store.TreeParam, truncated bool, err error) {
	deadline := time.Now().Add(walkBudget)
	type item struct {
		path  string
		depth int
	}
	queue := []item{{o.Root, 0}}
	tables := map[string]int{} // instances queued per table
	gpn := 0
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if gpn >= maxWalkGPN || time.Now().After(deadline) || len(params) >= store.MaxTreeParams {
			truncated = true
			break
		}
		it := queue[0]
		queue = queue[1:]
		gpn++
		names, f, err := ss.gpn(ctx, it.path, true)
		if err != nil {
			if len(params) == 0 {
				return nil, false, err
			}
			return params, true, nil
		}
		if f != nil {
			if it.path == o.Root && len(params) == 0 {
				return nil, false, fmt.Errorf("the box refused to list %s: %s", o.Root, f.Text())
			}
			continue // an object that vanished (9005) or is refused: leave it out
		}
		if len(names) > maxNamesPerReply {
			names, truncated = names[:maxNamesPerReply], true
		}
		leaves := 0
		for _, n := range names {
			name := n.Name
			if name == it.path || !strings.HasPrefix(name, it.path) || len(name) > 256 || hasControl(name) {
				continue
			}
			if strings.HasSuffix(name, ".") {
				if it.depth+1 >= walkDepth {
					continue
				}
				if numericLast(name) {
					tbl := it.path
					if tables[tbl] >= walkInstances {
						continue
					}
					tables[tbl]++
				}
				queue = append(queue, item{name, it.depth + 1})
				continue
			}
			if leaves >= maxLeavesPerObject || len(params) >= store.MaxTreeParams {
				truncated = true
				continue
			}
			leaves++
			params = append(params, store.TreeParam{Path: name, Writable: n.Writable, Secret: secretLeaf(name)})
		}
	}
	if len(queue) > 0 {
		truncated = true
	}
	s.readTreeValues(ctx, ss, o, params, deadline)
	return params, truncated, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// numericLast says whether the last segment of an object path is a table instance number ("...Host.3.").
func numericLast(name string) bool {
	n := strings.TrimSuffix(name, ".")
	n = n[strings.LastIndex(n, ".")+1:]
	_, err := strconv.Atoi(n)
	return err == nil && n != ""
}

// readTreeValues reads the values of the parameters worth keeping into params, in chunks, never a secret one. The
// leaves the resolver needs (and the names that tell the WAN interface apart) go first. A chunk the box refuses is
// skipped. It reads at most maxWalkGPV chunks.
func (s *Service) readTreeValues(ctx context.Context, ss *session, o store.ONU, params []store.TreeParam, deadline time.Time) {
	idx := map[string]int{}
	for i, p := range params {
		idx[p.Path] = i
	}
	first := map[string]bool{}
	for _, ps := range Resolve(o.Root, params).Paths {
		for _, p := range ps {
			first[p] = true
		}
	}
	var prio, rest []string
	for _, p := range params {
		if p.Secret {
			continue
		}
		leaf := leafOf(p.Path)
		if first[p.Path] || ((leaf == "Name" || leaf == "Alias" || leaf == "Status" || leaf == "ConnectionStatus") && strings.Contains(p.Path, "IP.Interface.")) {
			prio = append(prio, p.Path)
		} else {
			rest = append(rest, p.Path)
		}
	}
	names := append(prio, rest...)
	for n := 0; n < maxWalkGPV && len(names) > 0; n++ {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		k := walkChunk
		if k > len(names) {
			k = len(names)
		}
		chunk := names[:k]
		names = names[k:]
		vals, f, err := ss.gpv(ctx, chunk)
		if err != nil {
			return
		}
		if f != nil {
			continue
		}
		for _, v := range vals {
			if i, ok := idx[v.Name]; ok && !params[i].Secret {
				params[i].Value = v.Value
			}
		}
	}
}

// ---- views ----

// TreeBrief is the one-line state of the tree for the ONU card.
type TreeBrief struct {
	Count     int
	WalkedAt  time.Time
	Truncated bool
	Reason    string
	Resolved  []TreeLine // what the resolver found, in a fixed order
	Gaps      []string
	Notes     []string
}

// TreeLine is one resolved category and its paths.
type TreeLine struct {
	Key   string
	Paths []string
}

var resolvedOrder = []string{"optical.object", "optical.rx", "optical.tx", "optical.temp", "optical.volt", "optical.bias",
	"wan.devices", "wan.rx_bytes", "wan.tx_bytes", "wan.status", "wan.link", "hosts.table", "wifi.ssid", "wifi.passphrase", "wifi.preshared_key", "diag"}

// TreeInfo returns the brief of the tree of an ONU.
func (s *Service) TreeInfo(onuID string) (TreeBrief, bool) {
	var t store.ONUTree
	var ok bool
	s.st.View(func(d *store.Data) { t, ok = d.ONUTreeOf(onuID); t = t.Clone() })
	if !ok {
		return TreeBrief{}, false
	}
	b := TreeBrief{Count: t.Count, WalkedAt: t.WalkedAt, Truncated: t.Truncated, Reason: t.Reason, Gaps: t.Resolved.Gaps, Notes: t.Resolved.Notes}
	for _, k := range resolvedOrder {
		if p := t.Resolved.Paths[k]; len(p) > 0 {
			b.Resolved = append(b.Resolved, TreeLine{Key: k, Paths: p})
		}
	}
	return b, true
}

// RedactedValue is what the dump shows in place of the value of a secret parameter.
const RedactedValue = "<redacted>"

// TreeParamView is one parameter of the tree as the API shows it.
type TreeParamView struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	Value    string `json:"value,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// TreeView is the answer of GET /api/v1/onus/{id}/paramtree.
type TreeView struct {
	ONUID     string             `json:"onu_id"`
	Serial    string             `json:"serial"`
	Root      string             `json:"root"`
	WalkedAt  time.Time          `json:"walked_at"`
	Reason    string             `json:"reason,omitempty"`
	SWVersion string             `json:"software_version,omitempty"`
	HWVersion string             `json:"hardware_version,omitempty"`
	Count     int                `json:"count"`
	Truncated bool               `json:"truncated"`
	Resolved  store.TreeResolved `json:"resolved"`
	Params    []TreeParamView    `json:"params"`
}

// ErrNoTree: the ONU is known but its tree has not been walked yet.
var ErrNoTree = errors.New("no parameter tree has been walked for that ONU yet")

// ParamTree returns the tree of an ONU for the API and the download. The value of a secret parameter is never in it:
// it is not stored, and a parameter whose name looks secret shows "<redacted>" whatever the row holds.
func (s *Service) ParamTree(onuID string) (TreeView, error) {
	var t store.ONUTree
	var haveTree bool
	var o store.ONU
	var haveONU bool
	s.st.View(func(d *store.Data) {
		if o, haveONU = d.ONUs[onuID]; haveONU {
			t, haveTree = d.ONUTreeOf(onuID)
			t = t.Clone()
		}
	})
	if !haveONU {
		return TreeView{}, ErrONUGone
	}
	if !haveTree {
		return TreeView{}, ErrNoTree
	}
	v := TreeView{ONUID: o.ID, Serial: o.Serial, Root: t.Root, WalkedAt: t.WalkedAt, Reason: t.Reason, SWVersion: t.SWVersion, HWVersion: t.HWVersion,
		Count: t.Count, Truncated: t.Truncated, Resolved: t.Resolved, Params: []TreeParamView{}}
	for _, p := range t.Params() {
		pv := TreeParamView{Path: p.Path, Writable: p.Writable, Value: p.Value}
		if p.Secret || secretLeaf(p.Path) {
			pv.Value, pv.Redacted = RedactedValue, true
		}
		v.Params = append(v.Params, pv)
	}
	return v, nil
}

// TreeText is the plain-text dump of a tree: one line per parameter, "path<TAB>rw|ro<TAB>value".
func TreeText(v TreeView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ONU %s (%s) parameter tree, root %s\n# walked %s (%s), %d parameters%s\n# values of passphrase, password and key parameters are withheld\n",
		v.Serial, v.ONUID, v.Root, v.WalkedAt.UTC().Format(time.RFC3339), v.Reason, v.Count, map[bool]string{true: ", stopped at a limit", false: ""}[v.Truncated])
	for _, p := range v.Params {
		rw := "ro"
		if p.Writable {
			rw = "rw"
		}
		b.WriteString(p.Path + "\t" + rw + "\t" + p.Value + "\n")
	}
	return b.String()
}

// MarkWalks marks every ONU that has been reporting and has no tree yet as needing a walk (the retroactive pass). It
// does not contact any box: each one is walked at its next ordinary check-in, within the walk limits. It is
// idempotent and says how many boxes it marked.
func (s *Service) MarkWalks() int {
	n := 0
	_ = s.st.Update(func(d *store.Data) error {
		for id, o := range d.ONUs {
			if o.LastInform.IsZero() || o.Root == "" || o.State.WalkWanted {
				continue
			}
			if _, has := d.ONUTrees[id]; has {
				continue
			}
			o.State.WalkWanted = true
			d.ONUs[id] = o
			n++
		}
		if n == 0 {
			return store.ErrNoChange
		}
		return nil
	})
	return n
}

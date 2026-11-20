package web

import (
	"strings"
	"time"

	"ispbill/internal/billing"
)

// The admin shell: which sidebar group a page belongs to, its breadcrumb and
// the two-word hero every page opens with (docs/release-plan-routers.md,
// section 4). Words here are what an operator reads, so they say "router".

// heroAct is one button on the right of the hero. Modal opens the dialog with
// that id; otherwise Href is a plain link.
type heroAct struct {
	Label, Href, Modal string
	Ghost              bool
}

type pageMeta struct {
	Group, Label, Href string
	A, B, Lead         string
}

var pageMetas = map[string]pageMeta{
	"dashboard":     {"", "Overview", "/admin", "", "", "A quick read on money, routers and who needs you."},
	"accounts":      {"Customers", "Subscribers", "/admin/accounts", "Your", "subscribers.", "Every flat on the network: what they pay for, when it ends and whether the router has caught up."},
	"apartments":    {"Customers", "Apartments", "/admin/apartments", "Your", "apartments.", "The blocks you serve, the routers that serve them, their flats and who looks after them."},
	"devices":       {"Customers", "New devices", "/admin/devices", "New", "devices.", "Phones, ONTs and laptops the routers have seen that no flat owns yet."},
	"live":          {"Network", "Live now", "/admin/live", "Online", "now.", "Who is connected right now and how hard they are pulling. Refreshes every 10 seconds."},
	"sites":         {"Network", "Routers", "/admin/routers", "Your", "routers.", "One MikroTik per site. Add one, paste a single line, and the server does the rest."},
	"fleet":         {"Network", "Routers", "/admin/routers", "Your", "routers.", "One MikroTik per site."},
	"packages":      {"Network", "Plans", "/admin/packages", "Your", "plans.", "Speeds, durations and prices. Keep prices unique: M-Pesa payments are matched to a plan by amount."},
	"map":           {"Network", "Network map", "/admin/map", "Network", "map.", "The server, each router and what hangs off it."},
	"equipment":     {"Network", "Equipment", "/admin/equipment", "Hardware", "inventory.", "ONTs, OLTs, routers and splitters: where each one is and who has it."},
	"metrics":       {"Network", "Usage and quality", "/admin/metrics", "Usage", "and quality.", "Traffic, latency and loss for the selected router."},
	"uptime":        {"Network", "Uptime", "/admin/uptime", "Router", "uptime.", "How often each router answered, and what happened when it did not."},
	"payments":      {"Finance", "Payments", "/admin/payments", "Payments", "ledger.", "Every shilling in, with the receipt it came with."},
	"record":        {"Finance", "Record payment", "/admin/payments/record", "Record", "payment.", "For M-Pesa money that reached the Paybill but did not apply by itself."},
	"unmatched":     {"Finance", "Unmatched", "/admin/unmatched", "Unmatched", "payments.", "Money that arrived but could not be given to a flat or plan."},
	"orders":        {"Finance", "Orders", "/admin/orders", "Portal", "orders.", "Checkouts started from the tenant portal."},
	"vouchers":      {"Finance", "Vouchers", "/admin/vouchers", "Prepaid", "vouchers.", "Printable codes that give a flat time without M-Pesa."},
	"messages":      {"Communications", "Messages", "/admin/messages", "Outgoing", "messages.", "WhatsApp notices to tenants: sent, waiting and failed."},
	"alerts":        {"Communications", "Alerts", "/admin/alerts", "Open", "alerts.", "What the monitor found and who was told."},
	"settings":      {"System", "Settings", "/admin/settings", "Server", "settings.", "Lifecycle rules, payments and backups."},
	"set-whatsapp":  {"System", "WhatsApp", "/admin/settings/whatsapp", "WhatsApp", "settings.", "Where tenant messages and alerts are sent from."},
	"set-mpesa":     {"System", "M-Pesa", "/admin/settings/mpesa", "M-Pesa", "settings.", "Daraja credentials, callback addresses and test payments."},
	"set-email":     {"System", "Email", "/admin/settings/email", "Email", "settings.", "Outgoing mail for receipts and alerts."},
	"set-equipment": {"System", "Equipment tools", "/admin/settings/equipment", "Equipment", "tools.", "Look up a serial or MAC against the vendor lists."},
	"security":      {"System", "Security", "/admin/security", "Account", "security.", "Password, authenticator, signed-in devices and who else can sign in."},
	"set-tools":     {"System", "Tools", "/admin/tools", "Router", "tools.", "Ping, trace and run a read-only command from the server."},
	"audit":         {"System", "Audit", "/admin/audit", "Audit", "trail.", "Who changed what, newest first."},
	"search":        {"", "Search", "/admin/search", "Search", "results.", ""},
}

func (p *adminPage) setHero(active string, now time.Time) {
	m, ok := pageMetas[active]
	if !ok {
		m = pageMeta{"", "Admin", "/admin", "Fast", "admin.", ""}
	}
	p.Group, p.Label, p.Href = m.Group, m.Label, m.Href
	p.H1a, p.H1b, p.Lead = m.A, m.B, m.Lead
	if active == "dashboard" {
		p.H1a, p.H1b = greeting(now, p.User)
	}
	if !p.CanWrite {
		return
	}
	switch active {
	case "dashboard", "accounts":
		p.Acts = []heroAct{{Label: "Add customer", Modal: "addcustomer"}}
		if active == "dashboard" {
			p.Acts = append(p.Acts, heroAct{Label: "Add router", Modal: "addrouter", Ghost: true})
		}
	case "sites", "fleet":
		p.Acts = []heroAct{{Label: "Add router", Modal: "addrouter"}}
	case "apartments":
		p.Acts = []heroAct{{Label: "New apartment", Href: "/admin/apartments/new"}}
	case "devices":
		p.Acts = []heroAct{{Label: "Refresh", Href: "/admin/devices", Ghost: true}}
	case "payments":
		p.Acts = []heroAct{{Label: "Record payment", Href: "/admin/payments/record"}, {Label: "Export CSV", Href: "/admin/payments.csv", Ghost: true}}
	case "packages":
		p.Acts = []heroAct{{Label: "New plan", Href: "#new"}}
	case "metrics":
		p.Acts = []heroAct{{Label: "Usage CSV", Href: "/admin/metrics.csv", Ghost: true}}
	}
}

// greeting is "Good evening," and the name, by the hour in Nairobi.
func greeting(now time.Time, user string) (string, string) {
	h := now.In(billing.EAT).Hour()
	a := "Good evening,"
	switch {
	case h < 4:
		a = "Still up,"
	case h < 12:
		a = "Good morning,"
	case h < 17:
		a = "Good afternoon,"
	}
	name := titleName(user)
	if name == "" {
		name = "there"
	}
	return a, name + "."
}

func titleName(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, ".@_- "); i > 0 {
		u = u[:i]
	}
	if u == "" {
		return ""
	}
	return strings.ToUpper(u[:1]) + strings.ToLower(u[1:])
}

func initialOf(u string) string {
	n := titleName(u)
	if n == "" {
		return "?"
	}
	return n[:1]
}

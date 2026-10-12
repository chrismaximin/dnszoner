package ui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/dns"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// acctField is one provider-specific setting in the account form.
type acctField struct {
	label, hint string
	secret      bool
	ptr         func(*config.Source) *string
}

var acctFields = map[string][]acctField{
	"route53": {
		{"Profile", "AWS shared config profile (~/.aws/config); empty uses the default credentials chain", false,
			func(s *config.Source) *string { return &s.Profile }},
		{"Region", "Optional: Route53 is global (default us-east-1)", false,
			func(s *config.Source) *string { return &s.Region }},
		{"Access key ID", "Optional static credentials; empty uses the profile or AWS_* environment variables", false,
			func(s *config.Source) *string { return &s.AccessKeyID }},
		{"Secret access key", "Required with an access key ID", true,
			func(s *config.Source) *string { return &s.SecretAccessKey }},
		{"Session token", "Only for temporary credentials", true,
			func(s *config.Source) *string { return &s.SessionToken }},
	},
	"cloudflare": {
		{"API token", "Create one under My Profile › API Tokens, with Zone › Zone › Read and Zone › DNS › Edit for the zones to manage", true,
			func(s *config.Source) *string { return &s.APIToken }},
	},
	"hetzner": {
		{"API token", "A Read & Write token from the Hetzner Console, under the project holding your zones › Security › API tokens (tokens from the old DNS Console don't work)", true,
			func(s *config.Source) *string { return &s.APIToken }},
	},
	"godaddy": {
		{"API key", "A Production key from developer.godaddy.com/keys. GoDaddy only opens its API to some accounts (by number of domains, or with a Discount Domain Club plan): an ACCESS_DENIED error means yours isn't one", false,
			func(s *config.Source) *string { return &s.APIKey }},
		{"API secret", "Shown once, when the key is created", true,
			func(s *config.Source) *string { return &s.APISecret }},
	},
	"ovh": {
		{"Endpoint", "ovh-eu (the default when empty), ovh-ca or ovh-us", false,
			func(s *config.Source) *string { return &s.Endpoint }},
		{"Application key", "Create the keys at eu.api.ovh.com/createToken (or ca. / us.), with GET, POST, PUT and DELETE on /domain/*", false,
			func(s *config.Source) *string { return &s.ApplicationKey }},
		{"Application secret", "From the same page", true,
			func(s *config.Source) *string { return &s.ApplicationSecret }},
		{"Consumer key", "From the same page", true,
			func(s *config.Source) *string { return &s.ConsumerKey }},
	},
	"namecheap": {
		{"API user", "Your Namecheap username; enable API access under Profile › Tools › API Access", false,
			func(s *config.Source) *string { return &s.APIUser }},
		{"API key", "From the same API Access page", true,
			func(s *config.Source) *string { return &s.APIKey }},
		{"Client IP", "The whitelisted IP calls come from; empty discovers your public IP", false,
			func(s *config.Source) *string { return &s.ClientIP }},
		{"API endpoint", "Empty for production; https://api.sandbox.namecheap.com/xml.response for the sandbox", false,
			func(s *config.Source) *string { return &s.APIEndpoint }},
	},
}

const acctLabelW = 19

var (
	stAcctLabel   = stLabel.Width(acctLabelW)
	stAcctLabelOn = stLabelOn.Width(acctLabelW)
)

type testResult struct {
	running bool
	zones   int
	err     error
}

type accountsState struct {
	cursor        int
	confirmDelete bool
	tests         map[string]testResult // by source name
	form          *accountForm
}

type accountForm struct {
	orig     string // name of the account being edited, empty for a new one
	provider int    // index into config.Providers
	name     textinput.Model
	inputs   map[string][]textinput.Model // by provider, so switching keeps values
	focus    int                          // 0 provider, 1 name, then the provider's fields
	reveal   bool
	err      string
	test     *testResult
}

// formTestKey is where the form's own connection test result goes.
const formTestKey = "\x00form"

func newInput(width int) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 512
	in.Width = width
	return in
}

func (m *Model) openAccounts() {
	if m.accounts == nil {
		m.accounts = &accountsState{tests: map[string]testResult{}}
	}
	m.accounts.confirmDelete = false
	m.accounts.cursor = min(m.accounts.cursor, max(0, len(m.opts.Config.Sources)-1))
	m.mode = modeAccounts
	m.setStatus("", false)
}

func (m *Model) openAccountForm(src *config.Source) tea.Cmd {
	w := max(20, m.width-acctLabelW-8)
	f := &accountForm{name: newInput(w), inputs: map[string][]textinput.Model{}}
	f.name.Placeholder = "e.g. AWS prod"
	cur := config.Source{Provider: "route53"}
	if src != nil {
		cur = *src
		f.orig = src.Name
		f.name.SetValue(src.Name)
		f.focus = 1
	}
	f.provider = max(0, slices.Index(config.Providers, cur.Provider))
	for _, p := range config.Providers {
		for _, fd := range acctFields[p] {
			in := newInput(w)
			if p == cur.Provider {
				in.SetValue(*fd.ptr(&cur))
			}
			if fd.secret {
				in.EchoMode = textinput.EchoPassword
				in.EchoCharacter = '•'
			}
			f.inputs[p] = append(f.inputs[p], in)
		}
	}
	m.accounts.form = f
	return m.focusAccountField(f.focus)
}

func (f *accountForm) providerName() string { return config.Providers[f.provider] }

func (f *accountForm) numFields() int { return 2 + len(acctFields[f.providerName()]) }

// input returns the text input for focus index i (1 and up).
func (f *accountForm) input(i int) *textinput.Model {
	if i == 1 {
		return &f.name
	}
	return &f.inputs[f.providerName()][i-2]
}

func (f *accountForm) source() config.Source {
	src := config.Source{Name: strings.TrimSpace(f.name.Value()), Provider: f.providerName()}
	for i, fd := range acctFields[src.Provider] {
		*fd.ptr(&src) = strings.TrimSpace(f.inputs[src.Provider][i].Value())
	}
	return src
}

func (m *Model) focusAccountField(i int) tea.Cmd {
	f := m.accounts.form
	if f.focus > 0 {
		f.input(f.focus).Blur()
	}
	f.focus = (i + f.numFields()) % f.numFields()
	if f.focus == 0 {
		return nil
	}
	in := f.input(f.focus)
	in.CursorEnd()
	return in.Focus()
}

func (m *Model) newProvider(src config.Source) dns.Provider {
	if m.opts.Fake {
		return &dns.Fake{Store: m.opts.Store, Source: src.Name}
	}
	return dns.New(src)
}

type testDoneMsg struct {
	key   string
	zones int
	err   error
}

func testCmd(key string, prov dns.Provider) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		zones, err := prov.ListZones(ctx)
		return testDoneMsg{key: key, zones: len(zones), err: err}
	}
}

func (m *Model) handleTestDone(msg testDoneMsg) {
	if m.accounts == nil {
		return
	}
	r := testResult{zones: msg.zones, err: msg.err}
	if msg.key == formTestKey {
		if m.accounts.form != nil {
			m.accounts.form.test = &r
		}
		return
	}
	m.accounts.tests[msg.key] = r
}

func (m *Model) updateAccounts(msg tea.Msg) tea.Cmd {
	a := m.accounts
	if a.form != nil {
		return m.updateAccountForm(msg)
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	srcs := m.opts.Config.Sources
	if a.confirmDelete {
		switch k.String() {
		case "y", "enter":
			a.confirmDelete = false
			return m.deleteAccount(srcs[a.cursor].Name)
		default:
			a.confirmDelete = false
		}
		return nil
	}
	switch k.String() {
	case "esc", "q", "ctrl+o":
		m.mode = modeSearch
	case "up", "k", "ctrl+p":
		a.cursor = max(0, a.cursor-1)
	case "down", "j", "ctrl+n":
		a.cursor = min(len(srcs)-1, a.cursor+1)
	case "a", "n":
		return m.openAccountForm(nil)
	case "enter", "e":
		if a.cursor < len(srcs) {
			src := srcs[a.cursor]
			return m.openAccountForm(&src)
		}
	case "d", "x", "delete":
		if a.cursor < len(srcs) {
			a.confirmDelete = true
		}
	case "t":
		if a.cursor < len(srcs) {
			name := srcs[a.cursor].Name
			a.tests[name] = testResult{running: true}
			return testCmd(name, m.opts.Providers[name])
		}
	case "s":
		if a.cursor < len(srcs) {
			if m.syncEvents != nil {
				m.setStatus("A sync is already running", false)
				return nil
			}
			return m.startSync(srcs[a.cursor].Name)
		}
	}
	return nil
}

func (m *Model) updateAccountForm(msg tea.Msg) tea.Cmd {
	f := m.accounts.form
	k, isKey := msg.(tea.KeyMsg)
	if isKey {
		switch k.String() {
		case "esc":
			m.accounts.form = nil
			return nil
		case "tab", "down":
			return m.focusAccountField(f.focus + 1)
		case "shift+tab", "up":
			return m.focusAccountField(f.focus - 1)
		case "enter", "ctrl+s":
			return m.saveAccount()
		case "ctrl+r":
			f.reveal = !f.reveal
			m.maskSecrets()
			return nil
		case "ctrl+t":
			src := f.source()
			if err := src.Validate(); err != nil && !strings.Contains(err.Error(), "name") {
				f.err = capitalize(err.Error())
				return nil
			}
			f.err = ""
			f.test = &testResult{running: true}
			return testCmd(formTestKey, m.newProvider(src))
		}
		f.err = ""
	}
	if f.focus == 0 {
		if isKey {
			switch k.Type {
			case tea.KeyLeft:
				f.provider = (f.provider + len(config.Providers) - 1) % len(config.Providers)
				f.test = nil
			case tea.KeyRight, tea.KeySpace:
				f.provider = (f.provider + 1) % len(config.Providers)
				f.test = nil
			}
		}
		return nil
	}
	var cmd tea.Cmd
	in := f.input(f.focus)
	*in, cmd = in.Update(msg)
	return cmd
}

// maskSecrets puts the echo mode of every field back in line with reveal.
func (m *Model) maskSecrets() {
	f := m.accounts.form
	for p, ins := range f.inputs {
		for i, fd := range acctFields[p] {
			switch {
			case !fd.secret || f.reveal:
				ins[i].EchoMode = textinput.EchoNormal
			default:
				ins[i].EchoMode = textinput.EchoPassword
			}
		}
	}
}

// writeConfig saves the accounts, except with --fake where they only live in
// memory.
func (m *Model) writeConfig(c *config.Config) error {
	if m.opts.Fake {
		return nil
	}
	return config.Save(m.opts.ConfigPath, c)
}

func storeSources(c *config.Config) []store.Source {
	out := make([]store.Source, len(c.Sources))
	for i, s := range c.Sources {
		out[i] = store.Source{Name: s.Name, Provider: s.Provider}
	}
	return out
}

func (m *Model) saveAccount() tea.Cmd {
	a, f := m.accounts, m.accounts.form
	src := f.source()
	if err := src.Validate(); err != nil {
		f.err = capitalize(err.Error())
		return nil
	}
	next := &config.Config{Sources: slices.Clone(m.opts.Config.Sources)}
	idx := slices.IndexFunc(next.Sources, func(s config.Source) bool { return f.orig != "" && s.Name == f.orig })
	for i, s := range next.Sources {
		if s.Name == src.Name && i != idx {
			f.err = fmt.Sprintf("An account named %q already exists", src.Name)
			return nil
		}
	}
	if idx >= 0 {
		next.Sources[idx] = src
	} else {
		idx = len(next.Sources)
		next.Sources = append(next.Sources, src)
	}
	if err := m.writeConfig(next); err != nil {
		f.err = "Saving the config: " + err.Error()
		return nil
	}

	st := m.opts.Store
	if f.orig != "" && f.orig != src.Name {
		if err := st.RenameSource(f.orig, src.Name); err != nil {
			f.err = "Renaming in the cache: " + err.Error()
			return nil
		}
		delete(m.opts.Providers, f.orig)
		delete(a.tests, f.orig)
	}
	if err := st.SetSources(storeSources(next)); err != nil {
		f.err = "Updating the cache: " + err.Error()
		return nil
	}
	m.opts.Config = next
	m.opts.Providers[src.Name] = m.newProvider(src)
	delete(a.tests, src.Name)
	a.form = nil
	a.cursor = idx
	m.reloadNow()

	status := "Saved account " + src.Name
	if m.opts.Fake {
		m.setStatus(status+" (not written to disk with --fake)", false)
		return nil
	}
	if m.syncEvents != nil {
		m.setStatus(status+"; press s to sync it once the current sync is done", false)
		return nil
	}
	m.setStatus(status+"; syncing it", false)
	return m.startSync(src.Name)
}

func (m *Model) deleteAccount(name string) tea.Cmd {
	next := &config.Config{}
	for _, s := range m.opts.Config.Sources {
		if s.Name != name {
			next.Sources = append(next.Sources, s)
		}
	}
	if err := m.writeConfig(next); err != nil {
		m.setStatus("Saving the config: "+err.Error(), true)
		return nil
	}
	if err := m.opts.Store.SetSources(storeSources(next)); err != nil {
		m.setStatus("Updating the cache: "+err.Error(), true)
		return nil
	}
	m.opts.Config = next
	delete(m.opts.Providers, name)
	delete(m.accounts.tests, name)
	m.accounts.cursor = min(m.accounts.cursor, max(0, len(next.Sources)-1))
	m.setStatus("Deleted account "+name, false)
	return m.reloadNow()
}

// accountStats counts the cached zones and pending changes of a source.
func (m *Model) accountStats(name string) (zones, pending int, synced time.Time) {
	ids := map[int64]bool{}
	for _, z := range m.zones {
		if z.Source == name {
			zones++
			ids[z.ID] = true
			if z.SyncedAt.After(synced) {
				synced = z.SyncedAt
			}
		}
	}
	for _, p := range m.pending {
		if ids[p.ZoneID] {
			pending++
		}
	}
	return zones, pending, synced
}

func accountDetails(s config.Source) string {
	var parts []string
	switch s.Provider {
	case "route53":
		switch {
		case s.AccessKeyID != "":
			parts = append(parts, "key "+maskKey(s.AccessKeyID))
		case s.Profile != "":
			parts = append(parts, "profile "+s.Profile)
		default:
			parts = append(parts, "default credentials")
		}
		if s.Region != "" {
			parts = append(parts, s.Region)
		}
	case "namecheap":
		parts = append(parts, "user "+s.APIUser)
		if strings.Contains(s.APIEndpoint, "sandbox") {
			parts = append(parts, "sandbox")
		}
	case "cloudflare", "hetzner":
		parts = append(parts, "token "+maskKey(s.APIToken))
	case "godaddy":
		parts = append(parts, "key "+maskKey(s.APIKey))
	case "ovh":
		ep := s.Endpoint
		if ep == "" {
			ep = "ovh-eu"
		}
		parts = append(parts, ep+", key "+maskKey(s.ApplicationKey))
	}
	return strings.Join(parts, ", ")
}

// maskKey shows the start and end of a key, like AKIA…WXYZ.
func maskKey(k string) string {
	if len(k) <= 8 {
		return "…"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "never synced"
	case d < time.Minute:
		return "synced just now"
	case d < time.Hour:
		return fmt.Sprintf("synced %dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("synced %dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("synced %dd ago", int(d.Hours()/24))
	}
}

func testMessage(r testResult) (string, lipgloss.Style) {
	switch {
	case r.running:
		return "testing…", stPending
	case r.err != nil:
		return "✗ " + r.err.Error(), stErr
	default:
		return fmt.Sprintf("✓ connected, %d zones", r.zones), stOK
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

func (m *Model) viewAccounts() string {
	if m.accounts.form != nil {
		return m.viewAccountForm()
	}
	a := m.accounts
	srcs := m.opts.Config.Sources
	var b strings.Builder
	where := tildePath(m.opts.ConfigPath)
	if m.opts.Fake {
		where = "in memory (--fake)"
	}
	b.WriteString(stTitle.Render(" Accounts") + "  " + stMuted.Render(where) + "\n\n")

	if len(srcs) == 0 {
		b.WriteString("  No accounts yet. Press " + stKey.Render("a") + " to add your AWS Route53, Cloudflare, Namecheap, GoDaddy, OVH or Hetzner account.\n")
	} else {
		nameW, provW, detW := 20, 12, max(16, min(30, m.width-70))
		b.WriteString(stHeader.Render("   "+pad("Name", nameW)+" "+pad("Provider", provW)+" "+pad("Details", detW)+" Cache") + "\n")
		for i, s := range srcs {
			zones, pending, synced := m.accountStats(s.Name)
			cache := fmt.Sprintf("%d zones · %s", zones, ago(synced))
			if pending > 0 {
				cache += " · " + stPending.Render(fmt.Sprintf("%d pending", pending))
			}
			row := pad(s.Name, nameW) + " " + pad(config.ProviderLabel[s.Provider], provW) + " " + stMuted.Render(pad(accountDetails(s), detW)) + " " + cache
			marker := "  "
			if i == a.cursor {
				marker = stTitle.Render("› ")
			}
			b.WriteString(" " + marker + row + "\n")
			indent := "   " + strings.Repeat(" ", nameW+1)
			if r, ok := a.tests[s.Name]; ok {
				msg, st := testMessage(r)
				b.WriteString(m.wrap(msg, indent, st))
			} else if e, ok := m.syncErrBySrc[s.Name]; ok {
				b.WriteString(m.wrap("✗ last sync failed: "+e, indent, stErr))
			}
		}
		if a.confirmDelete {
			s := srcs[a.cursor]
			zones, pending, _ := m.accountStats(s.Name)
			detail := fmt.Sprintf("Its %d cached zones", zones)
			if pending > 0 {
				detail += fmt.Sprintf(" and %d pending changes", pending)
			}
			detail += " are removed from the cache. Nothing changes at the provider."
			b.WriteString("\n" + m.wrap(fmt.Sprintf("Delete the account %q?", s.Name), " ", stErr) + m.wrap(detail, " ", stMuted) +
				" " + stKey.Render("y") + stMuted.Render(" delete · any other key: keep") + "\n")
		}
	}

	for i := strings.Count(b.String(), "\n"); i < m.height-2; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	b.WriteString(helpLine("a", "add", "enter", "edit", "d", "delete", "t", "test", "s", "sync", "esc", "back"))
	return b.String()
}

func (m *Model) viewAccountForm() string {
	f := m.accounts.form
	var b strings.Builder
	title := " New account"
	if f.orig != "" {
		title = " Edit account"
	}
	b.WriteString(stTitle.Render(title))
	if f.orig != "" {
		b.WriteString("  " + stZone.Render(f.orig))
	}
	b.WriteString("\n\n")

	row := func(i int, label, value, hint string) {
		marker, l := "  ", stAcctLabel.Render(label)
		if i == f.focus {
			marker, l = stTitle.Render("› "), stAcctLabelOn.Render(label)
		}
		b.WriteString(" " + marker + l + " " + value + "\n")
		if i == f.focus && hint != "" {
			b.WriteString(m.wrap(hint, strings.Repeat(" ", acctLabelW+4), stMuted))
		}
	}

	var choices []string
	for i, p := range config.Providers {
		if i == f.provider {
			choices = append(choices, stChoice.Render(" "+config.ProviderLabel[p]+" "))
		} else {
			choices = append(choices, stMuted.Render(" "+config.ProviderLabel[p]+" "))
		}
	}
	provHint := ""
	if f.focus == 0 {
		provHint = "←→ to choose"
	}
	row(0, "Provider", strings.Join(choices, " "), provHint)
	row(1, "Name", f.name.View(), "How the account is labelled next to its domains")
	for i, fd := range acctFields[f.providerName()] {
		row(i+2, fd.label, f.inputs[f.providerName()][i].View(), fd.hint)
	}

	if f.test != nil {
		msg, st := testMessage(*f.test)
		b.WriteString("\n" + m.wrap(msg, " ", st))
	}
	if f.err != "" {
		b.WriteString("\n" + m.wrap(f.err, " ", stErr))
	}
	if m.opts.Fake {
		b.WriteString("\n" + m.wrap("--fake: accounts are kept in memory and not written to the config file", " ", stPending))
	}

	for i := strings.Count(b.String(), "\n"); i < m.height-2; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	reveal := "show secrets"
	if f.reveal {
		reveal = "hide secrets"
	}
	b.WriteString(helpLine("tab", "next field", "enter", "save", "ctrl+t", "test connection", "ctrl+r", reveal, "esc", "cancel"))
	return b.String()
}

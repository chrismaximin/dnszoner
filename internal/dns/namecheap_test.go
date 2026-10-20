package dns

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// fakeNamecheap keeps one zone's hosts, like the Namecheap XML API.
type fakeNamecheap struct {
	mu        sync.Mutex
	hosts     []ncHost
	emailType string
}

func (f *fakeNamecheap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ParseForm()
	fail := func(msg string) {
		fmt.Fprintf(w, `<ApiResponse Status="ERROR"><Errors><Error Number="1011150">%s</Error></Errors></ApiResponse>`, msg)
	}
	if r.Form.Get("ApiKey") != "k" || r.Form.Get("ClientIp") != "198.51.100.1" {
		fail("Invalid request IP")
		return
	}
	switch r.Form.Get("Command") {
	case "namecheap.domains.getList":
		fmt.Fprint(w, `<ApiResponse Status="OK"><CommandResponse><DomainGetListResult>
			<Domain Name="Example.co.uk" IsOurDNS="true"/><Domain Name="elsewhere.org" IsOurDNS="false"/>
			</DomainGetListResult><Paging><TotalItems>2</TotalItems></Paging></CommandResponse></ApiResponse>`)
	case "namecheap.domains.dns.getHosts":
		if r.Form.Get("SLD") != "example" || r.Form.Get("TLD") != "co.uk" {
			fail("Domain not found")
			return
		}
		fmt.Fprint(w, `<ApiResponse Status="OK"><CommandResponse><DomainDNSGetHostsResult>`)
		for _, h := range f.hosts {
			fmt.Fprintf(w, `<host Name="%s" Type="%s" Address="%s" MXPref="%s" TTL="%d"/>`,
				h.Name, h.Type, html.EscapeString(h.Address), h.MXPref, h.TTL)
		}
		fmt.Fprint(w, `</DomainDNSGetHostsResult></CommandResponse></ApiResponse>`)
	case "namecheap.domains.dns.setHosts":
		f.hosts = nil
		for i := 1; r.Form.Has("HostName" + strconv.Itoa(i)); i++ {
			n := strconv.Itoa(i)
			ttl, _ := strconv.ParseInt(r.Form.Get("TTL"+n), 10, 64)
			f.hosts = append(f.hosts, ncHost{Name: r.Form.Get("HostName" + n), Type: r.Form.Get("RecordType" + n),
				Address: r.Form.Get("Address" + n), MXPref: r.Form.Get("MXPref" + n), TTL: ttl})
		}
		f.emailType = r.Form.Get("EmailType")
		fmt.Fprint(w, `<ApiResponse Status="OK"><CommandResponse><DomainDNSSetHostResult IsSuccess="true"/></CommandResponse></ApiResponse>`)
	default:
		fail("unknown command")
	}
}

func newFakeNamecheap() *fakeNamecheap {
	return &fakeNamecheap{hosts: []ncHost{
		{Name: "@", Type: "A", Address: "192.0.2.1", TTL: 1799},
		{Name: "@", Type: "A", Address: "192.0.2.2", TTL: 1799},
		{Name: "@", Type: "MX", Address: "mx.example.com.", MXPref: "10", TTL: 1800},
		{Name: "@", Type: "CAA", Address: `0 issue "letsencrypt.org"`, TTL: 1800},
		{Name: "www", Type: "CAA", Address: "0 issuewild letsencrypt.org", TTL: 1800},
		{Name: "old", Type: "TXT", Address: "v=x", TTL: 1800},
	}}
}

func testNamecheap(t *testing.T, src config.Source, fake *fakeNamecheap) {
	t.Helper()
	p := NewNamecheap(src)
	p.limiter = newLimiter(0)
	ctx := context.Background()

	zones, err := p.ListZones(ctx)
	if err != nil || len(zones) != 1 || zones[0].Name != "example.co.uk" {
		t.Fatalf("ListZones = %+v, %v", zones, err)
	}
	recs, err := p.GetRecords(ctx, zones[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, fmt.Sprintf("%s %s %s %d", r.Type, r.Host, r.Value, r.TTL))
	}
	want := `A @ 192.0.2.1 1799|A @ 192.0.2.2 1799|MX @ 10 mx.example.com. 1800|CAA @ 0 issue "letsencrypt.org" 1800|CAA www 0 issuewild "letsencrypt.org" 1800|TXT old v=x 1800`
	if strings.Join(got, "|") != want {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}

	errs := p.Apply(ctx, zones[0], []Change{
		{Kind: store.PendingUpdate, Old: recs[0], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 1799}},
		{Kind: store.PendingUpdate, Old: recs[2], New: store.RecordData{Type: "MX", Host: "@", Value: "20 mx2.example.com", TTL: 1800}},
		{Kind: store.PendingDelete, Old: recs[5]},
		{Kind: store.PendingUpdate, Old: recs[4], New: store.RecordData{Type: "CAA", Host: "www", Value: `0 issuewild "sectigo.com"`, TTL: 1800}},
		{Kind: store.PendingCreate, New: store.RecordData{Type: "CNAME", Host: "www", Value: "example.co.uk.", TTL: 300}},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	got = nil
	for _, h := range fake.hosts {
		got = append(got, fmt.Sprintf("%s %s %s %s %d", h.Type, h.Name, h.MXPref, h.Address, h.TTL))
	}
	slices.Sort(got)
	want = `A @  192.0.2.2 1799|A @  192.0.2.9 1799|CAA @  0 issue "letsencrypt.org" 1800|CAA www  0 issuewild "sectigo.com" 1800|CNAME www  example.co.uk. 300|MX @ 20 mx2.example.com. 1800`
	if strings.Join(got, "|") != want {
		t.Errorf("hosts after apply:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}
	if fake.emailType != "MX" {
		t.Errorf("EmailType = %q, want MX", fake.emailType)
	}

	errs = p.Apply(ctx, zones[0], []Change{{Kind: store.PendingDelete, Old: recs[5]}})
	if errs[0] != ErrConflict {
		t.Errorf("deleting a gone record: %v", errs[0])
	}
}

func TestNamecheap(t *testing.T) {
	fake := newFakeNamecheap()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	testNamecheap(t, config.Source{APIUser: "me", APIKey: "k", ClientIP: "198.51.100.1", APIEndpoint: srv.URL}, fake)

	bad := NewNamecheap(config.Source{APIUser: "me", APIKey: "k", ClientIP: "203.0.113.5", APIEndpoint: srv.URL})
	bad.limiter = newLimiter(0)
	if _, err := bad.ListZones(context.Background()); err == nil || !strings.Contains(err.Error(), "Invalid request IP") {
		t.Errorf("unlisted IP: %v", err)
	}
}

func TestNamecheapHTTPProxy(t *testing.T) {
	fake := newFakeNamecheap()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied++
		r.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	testNamecheap(t, config.Source{APIUser: "me", APIKey: "k", ClientIP: "198.51.100.1", APIEndpoint: srv.URL, Proxy: proxy.URL}, fake)
	if proxied == 0 {
		t.Error("no call went through the proxy")
	}
}

// TestSSHHelper stands in for ssh -W when the SSH proxy tests run it as
// "ssh": it connects to the -W address and relays stdin and stdout.
func TestSSHHelper(t *testing.T) {
	if os.Getenv("DNSZONER_FAKE_SSH") == "" {
		t.Skip("helper process")
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	os.WriteFile(os.Getenv("DNSZONER_FAKE_SSH"), []byte(strings.Join(args, " ")), 0o600)
	if os.Getenv("DNSZONER_FAKE_SSH_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "Host key verification failed.")
		os.Exit(255)
	}
	c, err := net.Dial("tcp", args[slices.Index(args, "-W")+1])
	if err != nil {
		os.Exit(255)
	}
	go func() { io.Copy(c, os.Stdin); c.Close() }()
	io.Copy(os.Stdout, c)
	os.Exit(0)
}

// fakeSSH puts an "ssh" running TestSSHHelper first in PATH and returns the
// file it writes its arguments to.
func fakeSSH(t *testing.T) string {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestSSHHelper$' -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	argsFile := filepath.Join(dir, "args")
	t.Setenv("DNSZONER_FAKE_SSH", argsFile)
	return argsFile
}

func TestNamecheapSSHProxy(t *testing.T) {
	argsFile := fakeSSH(t)
	fake := newFakeNamecheap()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	testNamecheap(t, config.Source{APIUser: "me", APIKey: "k", ClientIP: "198.51.100.1", APIEndpoint: srv.URL, Proxy: "ssh://deploy@vps.example.net:2222"}, fake)
	args, _ := os.ReadFile(argsFile)
	want := fmt.Sprintf("-o BatchMode=yes -o ExitOnForwardFailure=yes -W %s -l deploy -p 2222 -- vps.example.net", srv.Listener.Addr())
	if string(args) != want {
		t.Errorf("ssh args = %q, want %q", args, want)
	}

	t.Setenv("DNSZONER_FAKE_SSH_FAIL", "1")
	p := NewNamecheap(config.Source{APIUser: "me", APIKey: "k", ClientIP: "198.51.100.1", APIEndpoint: srv.URL, Proxy: "ssh://vps"})
	p.limiter = newLimiter(0)
	if _, err := p.ListZones(context.Background()); err == nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Errorf("failing ssh: %v", err)
	}
}

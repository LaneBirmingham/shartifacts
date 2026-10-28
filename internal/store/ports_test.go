package store

import (
	"errors"
	"sync"
	"testing"
)

func TestPortClaims(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, created, err := s.ClaimPort(PortClaim{Host: " MyServer ", Port: 8123, Service: "grafana", Owner: "Codex"}, 0)
	if err != nil || !created || c.Host != "myserver" || c.Protocol != "tcp" || c.ClaimedAt.IsZero() {
		t.Fatal(c, created, err)
	}
	// The same service claiming again updates its claim and keeps when it was made.
	again, created, err := s.ClaimPort(PortClaim{Host: "myserver", Port: 8123, Service: "Grafana", Note: "dashboards"}, 0)
	if err != nil || created || again.Note != "dashboards" || again.Owner != "Codex" || !again.ClaimedAt.Equal(c.ClaimedAt) {
		t.Fatal(again, created, err)
	}
	// Another service is refused, told who holds the port and where the next free one is.
	if _, _, err := s.ClaimPort(PortClaim{Host: "myserver", Port: 8124, Service: "loki"}, 0); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.ClaimPort(PortClaim{Host: "myserver", Port: 8123, Service: "prometheus"}, 0)
	var taken *PortTakenError
	if !errors.As(err, &taken) || !errors.Is(err, ErrConflict) || taken.Holder.Service != "Grafana" || taken.Next != 8125 {
		t.Fatalf("taken port: %v", err)
	}
	// The same number on another protocol or machine is a different port.
	for _, in := range []PortClaim{{Host: "myserver", Port: 8123, Protocol: "UDP", Service: "statsd"}, {Host: "192.168.1.20", Port: 8123, Service: "prometheus"}} {
		if _, created, err := s.ClaimPort(in, 0); err != nil || !created {
			t.Fatal(in, err)
		}
	}
	// Without a port, a claim takes the lowest free one at or above from.
	next, _, err := s.ClaimPort(PortClaim{Host: "myserver", Service: "tempo"}, 8123)
	if err != nil || next.Port != 8125 {
		t.Fatal(next, err)
	}
	if first, _, err := s.ClaimPort(PortClaim{Host: "laptop", Service: "dev server"}, 0); err != nil || first.Port != DefaultPortFrom {
		t.Fatal(first, err)
	}

	for _, in := range []PortClaim{
		{Host: "", Port: 1, Service: "x"}, {Host: "bad host", Port: 1, Service: "x"}, {Host: "a/b", Port: 1, Service: "x"},
		{Host: "h", Port: 70000, Service: "x"}, {Host: "h", Port: -1, Service: "x"}, {Host: "h", Port: 1, Service: "  "},
		{Host: "h", Port: 1, Service: "x", Protocol: "sctp"},
	} {
		if _, _, err := s.ClaimPort(in, 0); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	if _, _, err := s.ClaimPort(PortClaim{Host: "h", Service: "x"}, 70000); err == nil {
		t.Error("accepted from 70000")
	}

	list, err := s.Ports("MYSERVER")
	if err != nil || len(list) != 4 || list[0].Port != 8123 || list[0].Protocol != "tcp" || list[1].Protocol != "udp" || list[3].Port != 8125 {
		t.Fatal(list, err)
	}
	if hosts := s.PortHosts(); len(hosts) != 3 || hosts[0] != "192.168.1.20" || hosts[2] != "myserver" {
		t.Fatal(hosts)
	}

	// Claims outlive a restart; released ones do not.
	if old, ok, err := s.ReleasePort("myserver", 8124, ""); err != nil || !ok || old.Service != "loki" {
		t.Fatal(old, ok, err)
	}
	if _, ok, err := s.ReleasePort("myserver", 8124, "tcp"); err != nil || ok {
		t.Fatal("released twice", err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := s.Ports("")
	if len(all) != 5 {
		t.Fatal(all)
	}
	for _, c := range all {
		if c.Host == "myserver" && c.Port == 8124 {
			t.Fatal("a released claim came back", c)
		}
	}
}

// Agents asking for a free port at the same moment each get one of their own.
func TestPortClaimsDoNotCollide(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([]int, 20)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := s.ClaimPort(PortClaim{Host: "myserver", Service: "svc" + string(rune('a'+i))}, 9000)
			if err != nil {
				t.Error(err)
			}
			got[i] = c.Port
		}()
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, p := range got {
		if p < 9000 || p >= 9020 || seen[p] {
			t.Fatal("ports handed out twice or out of range:", got)
		}
		seen[p] = true
	}
}

package config

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, args []string, env map[string]string) (*Config, error) {
	t.Helper()
	return Parse(args, func(k string) string { return env[k] }, io.Discard)
}

func TestDefaults(t *testing.T) {
	c, err := parse(t, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Data != "./data" || c.AppProcs != 16 || c.AppIdle != 15*time.Minute || c.AppKeepVersions != 20 || c.AppDataQuota != 0 || c.Mode() != "private" {
		t.Fatalf("%+v", c)
	}
	if len(c.AllowedHosts) != 0 || c.ShareOrigin != "" {
		t.Fatalf("%+v", c)
	}
}

func TestEnvironment(t *testing.T) {
	env := map[string]string{
		"SHARTIFACTS_LISTEN":        "127.0.0.1:9000",
		"SHARTIFACTS_DATA":          "/srv/data",
		"SHARTIFACTS_ALLOWED_HOSTS": " Box.Local , https://box.tailnet.ts.net:443/, ",
		"SHARTIFACTS_SHARE_ORIGIN":  "share.example.com/",
		"SHARTIFACTS_APP_ORIGIN":    "https://*.Share.Example.com",
		"SHARTIFACTS_AUTH_PROXY":    "true",
		"SHARTIFACTS_APP_IDLE":      "2m",
		"SHARTIFACTS_RUNNER_URL":    "", // empty is unset
	}
	c, err := parse(t, []string{"-data", "/flag/wins"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || c.Data != "/flag/wins" || !c.AuthProxy || c.AppIdle != 2*time.Minute || c.RunnerURL != "" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.AllowedHosts, ",") != "box.local,box.tailnet.ts.net" {
		t.Fatal(c.AllowedHosts)
	}
	if c.ShareOrigin != "https://share.example.com" || c.AppOrigin != "https://*.share.example.com" || c.Mode() != "public-sharing" || c.ShareListen != ":8081" {
		t.Fatalf("%+v", c)
	}
	if c, err := parse(t, nil, map[string]string{"SHARTIFACTS_SHARE_ORIGIN": "share.example.com", "SHARTIFACTS_SHARE_LISTEN": "off"}); err != nil || c.ShareListen != "" {
		t.Fatal(c, err)
	}
	if _, err := parse(t, nil, map[string]string{"SHARTIFACTS_APP_PROCS": "many"}); err == nil || !strings.Contains(err.Error(), "SHARTIFACTS_APP_PROCS") {
		t.Fatal(err)
	}
}

func TestAppStorageSettings(t *testing.T) {
	c, err := parse(t, nil, map[string]string{"SHARTIFACTS_APP_KEEP_VERSIONS": "5", "SHARTIFACTS_APP_DATA_QUOTA": "2GiB"})
	if err != nil {
		t.Fatal(err)
	}
	if c.AppKeepVersions != 5 || c.AppDataQuota != 2<<30 {
		t.Fatalf("%+v", c)
	}
	for in, want := range map[string]ByteSize{"0": 0, "1048576": 1 << 20, "500MB": 500e6, "500 mb": 500e6, "1.5G": 3 << 29, "64k": 64 << 10, "2TiB": 2 << 40} {
		var b ByteSize
		if err := b.Set(in); err != nil || b != want {
			t.Errorf("%q: %d, %v; want %d", in, b, err, want)
		}
	}
	for _, in := range []string{"lots", "-1G", "5 parsecs", "1e30"} {
		var b ByteSize
		if err := b.Set(in); err == nil {
			t.Errorf("%q: accepted as %d", in, b)
		}
	}
	if s := ByteSize(2 << 30); s.String() != "2GiB" {
		t.Error(s.String())
	}
	// Keeping no versions would leave nothing to roll back to, and nothing live.
	for _, keep := range []string{"0", "-3"} {
		if _, err := parse(t, []string{"-app-keep-versions", keep}, nil); err == nil || !strings.Contains(err.Error(), "app-keep-versions") {
			t.Fatal(keep, err)
		}
	}
	if _, err := parse(t, nil, map[string]string{"SHARTIFACTS_APP_DATA_QUOTA": "plenty"}); err == nil || !strings.Contains(err.Error(), "SHARTIFACTS_APP_DATA_QUOTA") {
		t.Fatal(err)
	}
}

func TestDefaultPorts(t *testing.T) {
	c, err := parse(t, []string{"-share-origin", "https://share.example.com:443", "-app-origin", "https://*.share.example.com:443", "-public-origin", "http://box.lan:80"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.ShareOrigin != "https://share.example.com" || c.AppOrigin != "https://*.share.example.com" || c.PublicOrigin != "http://box.lan" {
		t.Fatalf("%+v", c)
	}
	if c, _ := parse(t, []string{"-share-origin", "https://share.example.com:8443"}, nil); c.ShareOrigin != "https://share.example.com:8443" {
		t.Fatal(c.ShareOrigin)
	}
}

func TestPublicOriginIsAllowed(t *testing.T) {
	c, err := parse(t, []string{"-public-origin", "https://box.tailnet.ts.net"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.AllowedHosts, ",") != "box.tailnet.ts.net" {
		t.Fatal(c.AllowedHosts)
	}
	if c.Mode() != "private" || !strings.Contains(strings.Join(c.Summary(), "\n"), "https://box.tailnet.ts.net") {
		t.Fatal(c.Summary())
	}
}

func TestRefusesUnsafeOrigins(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-share-origin", "ftp://share.example.com"}, "not an origin"},
		{[]string{"-share-origin", "https://share.example.com/s"}, "no path"},
		{[]string{"-public-origin", "https://*.example.com"}, "wildcard"},
		{[]string{"-app-origin", "https://*.share.example.com"}, "needs -share-origin"},
		{[]string{"-share-origin", "share.example.com", "-app-origin", "https://apps.example.com"}, "leading wildcard"},
		{[]string{"-share-origin", "share.example.com", "-app-origin", "https://*.com"}, "leading wildcard"},
		// The internet must never reach the workspace by the share hostname.
		{[]string{"-share-origin", "box.example.com", "-public-origin", "https://box.example.com"}, "workspace hostname"},
		{[]string{"-share-origin", "box.example.com", "-allowed-hosts", "box.example.com"}, "workspace hostname"},
		// Nor may the app wildcard swallow the workspace or the share hostname.
		{[]string{"-share-origin", "share.example.com", "-app-origin", "*.example.com"}, "app hostnames"},
		{[]string{"-share-origin", "share.example.net", "-app-origin", "*.example.com", "-public-origin", "box.example.com"}, "app hostnames"},
		// Private app addresses: a wildcard of their own, apart from the public one, that
		// takes in neither the workspace nor the share hostname.
		{[]string{"-private-app-origin", "https://apps.example.com"}, "leading wildcard"},
		{[]string{"-private-app-origin", "https://*.example.com", "-public-origin", "https://box.example.com"}, "app hostnames"},
		{[]string{"-private-app-origin", "https://*.example.com", "-share-origin", "share.example.com"}, "app hostnames"},
		{[]string{"-share-origin", "share.example.com", "-app-origin", "*.share.example.com", "-private-app-origin", "*.share.example.com"}, "same hostnames"},
		{[]string{"stray"}, "unexpected argument"},
	} {
		if _, err := parse(t, tc.args, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	// A dedicated domain: share on the apex, apps one label under it.
	if _, err := parse(t, []string{"-share-origin", "example.net", "-app-origin", "*.example.net", "-public-origin", "box.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	// Private app addresses need no sharing, and sit beside public ones.
	if c, err := parse(t, []string{"-private-app-origin", "apps.example.com", "-public-origin", "box.example.com"}, nil); err == nil {
		t.Fatalf("a private wildcard without its *: %+v", c)
	}
	if c, err := parse(t, nil, map[string]string{"SHARTIFACTS_PRIVATE_APP_ORIGIN": "*.apps.example.com/", "SHARTIFACTS_PUBLIC_ORIGIN": "box.example.com"}); err != nil || c.PrivateAppOrigin != "https://*.apps.example.com" || c.Mode() != "private" {
		t.Fatalf("private app origin: %+v %v", c, err)
	}
	if _, err := parse(t, []string{"-share-origin", "share.example.com", "-app-origin", "*.share.example.com", "-private-app-origin", "*.apps.example.com", "-public-origin", "box.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	// Local testing: browsers resolve every *.localhost name to this machine.
	if _, err := parse(t, []string{"-share-origin", "http://localhost:9000", "-app-origin", "http://*.localhost:9000"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUsage(t *testing.T) {
	var out strings.Builder
	if _, err := Parse([]string{"-help"}, func(string) string { return "" }, &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SHARTIFACTS_SHARE_ORIGIN") || !strings.Contains(out.String(), "-auth-proxy") {
		t.Fatal(out.String())
	}
	if _, err := Parse([]string{"-nope"}, func(string) string { return "" }, io.Discard); !errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

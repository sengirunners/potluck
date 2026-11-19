package taskproxy_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/potluck/taskproxy"
)

func TestConfig(t *testing.T) {
	o := taskproxy.Options{Profile: "development", DownloadMbit: 48, AllowHosts: []string{"Mirror.Example.COM.", "github.com", "*.cdn.example.com"}}
	b, err := o.Config()
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hosts        []string
		DownloadMbit int `json:"download_mbit"`
	}
	if err := json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	if config.DownloadMbit != 48 || !slices.Contains(config.Hosts, "mirror.example.com") || !slices.Contains(config.Hosts, "*.cdn.example.com") || len(config.Hosts) != len(taskproxy.DevelopmentHosts())+2 {
		t.Fatalf("%s", b)
	}
	for _, host := range []string{"", "127.0.0.1", "127.1", "0x7f.0x1", "*", "*.com", "*.workers.dev", "*.githubusercontent.com", "*.amazonaws.com", "*.s3.us-east-1.amazonaws.com", "*.core.windows.net", "*.blob.core.windows.net", "*.windows.net", "*.co.uk", "127.0.0.1.nip.io", "*.sslip.io", "example.com:443", "localhost", "a.com\nX: y", "bad..com"} {
		o.AllowHosts = []string{host}
		if _, err := o.Config(); err == nil {
			t.Errorf("accepted %q", host)
		}
	}
	for _, host := range []string{"*.cdn.example.com", "*.example.co.uk", "*.actions.githubusercontent.com", "my.worker.workers.dev", "cdn.example.com"} {
		o.AllowHosts = []string{host}
		if _, err := o.Config(); err != nil {
			t.Errorf("refused scoped entry %q: %v", host, err)
		}
	}
	for _, o := range []taskproxy.Options{{}, {Profile: "other"}, {Profile: "development", DownloadMbit: -1}, {Profile: "development", DownloadMbit: 100001}} {
		if _, err := o.Config(); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
}

func TestProxyBehavior(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for the cooperative proxy")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "proxy.py")
	if err := os.WriteFile(script, []byte(taskproxy.Script), 0600); err != nil {
		t.Fatal(err)
	}
	tests, err := filepath.Abs("proxy_test.py")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, tests, script)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

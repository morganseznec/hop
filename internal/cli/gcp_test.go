package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/config"
	"github.com/morganseznec/hop/internal/inventory"
)

// fakeGCP plays Resource Manager and Compute Engine for three projects: acme-shop has web
// (running) and db (stopped), acme-data has Compute Engine off, globex-ops has tools.
type fakeGCP struct {
	mu    sync.Mutex
	calls []string
	// web is what the lookup of web returns: "" as listed, "gone", "replaced" or
	// "stopped".
	web string
}

func (f *fakeGCP) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const (
	gcpWeb   = `{"id":"101","name":"web","zone":"https://www.googleapis.com/compute/v1/projects/acme-shop/zones/europe-west1-b","status":"RUNNING","labels":{"env":"prod"},"networkInterfaces":[{"networkIP":"10.132.0.2"}]}`
	gcpDB    = `{"id":"102","name":"db","zone":"https://www.googleapis.com/compute/v1/projects/acme-shop/zones/europe-west1-b","status":"TERMINATED","networkInterfaces":[{"networkIP":"10.132.0.3"}]}`
	gcpTools = `{"id":"301","name":"tools","zone":"https://www.googleapis.com/compute/v1/projects/globex-ops/zones/us-central1-a","status":"RUNNING","networkInterfaces":[{"networkIP":"10.128.0.9"}]}`
)

func (f *fakeGCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.Path)
	web := f.web
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer fictional-token" {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":401,"message":"Request had invalid authentication credentials."}}`)
		return
	}
	switch r.URL.Path {
	case "/v1/projects":
		io.WriteString(w, `{"projects":[{"projectId":"acme-shop"},{"projectId":"acme-data"},{"projectId":"globex-ops"}]}`)
	case "/compute/v1/projects/acme-shop/aggregated/instances":
		io.WriteString(w, `{"items":{"zones/europe-west1-b":{"instances":[`+gcpWeb+`,`+gcpDB+`]},"zones/us-east1-b":{"warning":{"code":"NO_RESULTS_ON_PAGE"}}}}`)
	case "/compute/v1/projects/acme-data/aggregated/instances":
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"message":"Compute Engine API has not been used in project acme-data before or it is disabled."}}`)
	case "/compute/v1/projects/globex-ops/aggregated/instances":
		io.WriteString(w, `{"items":{"zones/us-central1-a":{"instances":[`+gcpTools+`]}}}`)
	case "/compute/v1/projects/acme-shop/zones/europe-west1-b/instances/web":
		switch web {
		case "gone":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":404,"message":"The resource was not found"}}`)
		case "replaced":
			io.WriteString(w, strings.Replace(gcpWeb, `"101"`, `"199"`, 1))
		case "stopped":
			io.WriteString(w, strings.Replace(gcpWeb, "RUNNING", "TERMINATED", 1))
		default:
			io.WriteString(w, gcpWeb)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
	}
}

// gcpEnv sets up the AWS fleet (all fresh), GCP turned on for the given projects, a
// fake gcloud first on PATH, and the Google APIs on a local server. The gcloud
// arguments of a shell land in out/gcloud.
func gcpEnv(t *testing.T, projects ...string) (f *fakeGCP, out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gcloud is a shell script")
	}
	fleetEnv(t)
	terminal(t, false, true)
	if err := (&config.Config{Labels: map[string]string{"acme-prod": "ACME"}, GCPProjects: projects}).Save(); err != nil {
		t.Fatal(err)
	}
	bin, out := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
"auth print-access-token") echo fictional-token ;;
"compute ssh") printf '%s\n' "$@" > "$HOP_TEST_OUT/gcloud" ;;
*) echo "unexpected gcloud $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("HOP_TEST_OUT", out)
	f = &fakeGCP{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	saved := inventory.GCPEndpoints
	inventory.GCPEndpoints.Compute, inventory.GCPEndpoints.ResourceManager = srv.URL, srv.URL
	t.Cleanup(func() { inventory.GCPEndpoints = saved })
	return f, out
}

func TestGCPList(t *testing.T) {
	f, _ := gcpEnv(t, "*")
	out, stderr, err := runHop(t, "ls")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, want := range []string{"acme-p-tools", "web", "gcp:acme-shop", "europe-west1-b", "10.132.0.2", "tools", "gcp:globex-ops", "us-central1-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("hop ls does not show %q:\n%s", want, out)
		}
	}
	// db is stopped; acme-data has Compute Engine off, which is no error.
	if strings.Contains(out, "db") || strings.Contains(stderr, "acme-data") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, stderr)
	}
	if !f.called("/v1/projects") {
		t.Error(`"*" must list the projects`)
	}
	out, _, _ = runHop(t, "ls", "--all")
	if !strings.Contains(out, "db") || !strings.Contains(out, "stopped") {
		t.Errorf("hop ls --all:\n%s", out)
	}
}

func TestGCPProjectsWithoutPattern(t *testing.T) {
	f, _ := gcpEnv(t, "globex-ops")
	out, stderr, err := runHop(t, "ls")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.Contains(out, "gcp:globex-ops") || strings.Contains(out, "gcp:acme-shop") {
		t.Errorf("hop ls:\n%s", out)
	}
	if f.called("/v1/projects") {
		t.Error("projects listed although none was a pattern")
	}
}

func TestGCPProfileFilter(t *testing.T) {
	gcpEnv(t, "*")
	out, _, err := runHop(t, "ls", "-p", "gcp:globex-ops")
	if err != nil || !strings.Contains(out, "tools") || strings.Contains(out, "web") || strings.Contains(out, "acme-prod") {
		t.Errorf("-p gcp:globex-ops: %v\n%s", err, out)
	}
	out, _, err = runHop(t, "ls", "-p", "gcp")
	if err != nil || !strings.Contains(out, "web") || !strings.Contains(out, "gcp:globex-ops") || strings.Contains(out, "acme-prod") {
		t.Errorf("-p gcp: %v\n%s", err, out)
	}

	f, _ := gcpEnv(t, "*")
	out, _, err = runHop(t, "ls", "-p", "acme-prod")
	if err != nil || strings.Contains(out, "gcp:") || f.called("/") {
		t.Errorf("-p acme-prod must leave GCP out: %v\n%s", err, out)
	}
}

func TestGCPOff(t *testing.T) {
	fleetEnv(t)
	_, _, err := runHop(t, "ls", "-p", "gcp")
	if err == nil || !strings.Contains(err.Error(), "unknown profile: gcp (GCP is off: list the projects in gcp_projects in") {
		t.Errorf("error %v", err)
	}
}

func TestGCPShell(t *testing.T) {
	_, out := gcpEnv(t, "*")
	terminal(t, true, true)
	_, stderr, err := runHop(t, "web")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	args := readTestFile(t, filepath.Join(out, "gcloud"))
	if want := "compute\nssh\nweb\n--project=acme-shop\n--zone=europe-west1-b\n--tunnel-through-iap\n"; args != want {
		t.Errorf("gcloud arguments:\n%s\nwant:\n%s", args, want)
	}
	for _, want := range []string{"GCP does not record shell sessions", "session on web (gcp:acme-shop, europe-west1-b) through IAP"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
}

func TestGCPShellChecksAnOldInventory(t *testing.T) {
	cases := map[string]string{
		"gone":     "web (gcp:acme-shop, europe-west1-b) no longer exists",
		"replaced": "is now another instance of the same name",
		"stopped":  "web (gcp:acme-shop, europe-west1-b) is now stopped",
	}
	for state, want := range cases {
		t.Run(state, func(t *testing.T) {
			f, out := gcpEnv(t, "*")
			if _, _, err := runHop(t, "ls"); err != nil {
				t.Fatal(err)
			}
			// The listing is five minutes old: within the TTL, past the freshness window.
			ageCache(t, 5*time.Minute)
			f.mu.Lock()
			f.web = state
			f.mu.Unlock()
			terminal(t, true, true)
			_, _, err := runHop(t, "web")
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want %q", err, want)
			}
			if _, err := os.Stat(filepath.Join(out, "gcloud")); err == nil {
				t.Error("gcloud compute ssh ran")
			}
		})
	}
}

func TestGCPRefusesTheOtherCommands(t *testing.T) {
	_, out := gcpEnv(t, "*")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"run", "web", "uptime"}, "gcloud compute ssh web --project=acme-shop --zone=europe-west1-b --tunnel-through-iap --command '<command>'"},
		{[]string{"tunnel", "web", "8080"}, "gcloud compute start-iap-tunnel web <port>"},
		{[]string{"ssh-proxy", "web.hop", "22"}, "gcloud compute start-iap-tunnel web <port>"},
	}
	for _, c := range cases {
		_, _, err := runHop(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), "is a GCP instance: hop only opens shells there for now") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: error %v", c.args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "gcloud")); err == nil {
		t.Error("gcloud compute ssh ran")
	}
}

func TestGCPTranscriptRequire(t *testing.T) {
	_, out := gcpEnv(t, "*")
	if err := (&config.Config{GCPProjects: []string{"*"}, Transcript: config.TranscriptRequire}).Save(); err != nil {
		t.Fatal(err)
	}
	terminal(t, true, true)
	_, _, err := runHop(t, "web")
	if err == nil || !strings.Contains(err.Error(), "session refused (transcript: require): GCP does not record shell sessions") {
		t.Errorf("error %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "gcloud")); err == nil {
		t.Error("gcloud compute ssh ran")
	}
}

func TestGCPWithoutGcloud(t *testing.T) {
	gcpEnv(t, "*")
	t.Setenv("PATH", t.TempDir())
	out, stderr, err := runHop(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	// The AWS instances are still listed, and the cause is given.
	if !strings.Contains(out, "acme-p-tools") || strings.Contains(out, "gcp:") {
		t.Errorf("stdout:\n%s", out)
	}
	if !strings.Contains(stderr, "gcloud not found: install the Google Cloud CLI, then gcloud auth login") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestGCPDoctor(t *testing.T) {
	gcpEnv(t, "*")
	out, _, err := runHop(t, "doctor")
	if err != nil || !strings.Contains(out, "GCP support on (experimental)") || strings.Contains(out, "gcp *") {
		t.Errorf("%v\n%s", err, out)
	}
	t.Setenv("PATH", t.TempDir())
	out, _, _ = runHop(t, "doctor")
	if !strings.Contains(out, "gcloud not found: the GCP projects of gcp_projects cannot be listed") {
		t.Errorf("without gcloud:\n%s", out)
	}
}

func TestGCPProfileCompletion(t *testing.T) {
	gcpEnv(t, "*")
	profiles, _ := completeProfiles(&cobra.Command{}, nil, "")
	if !slices.Contains(profiles, "gcp\tGCP projects (experimental)") {
		t.Errorf("-p completion: %q", profiles)
	}
}

func TestGCPSortsAfterAWSByProject(t *testing.T) {
	gcpEnv(t, "*")
	out, _, err := runHop(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	pos := func(s string) int { return strings.Index(out, s) }
	// globex (AWS) comes before every GCP project, and the projects stay grouped.
	aws, shop, ops := pos("globex "), pos("gcp:acme-shop"), pos("gcp:globex-ops")
	if aws < 0 || shop < aws || ops < shop {
		t.Errorf("order:\n%s", out)
	}
}

func TestGCPExplainsAnEmptyScope(t *testing.T) {
	gcpEnv(t, "*")
	_, stderr, _ := runHop(t, "ls", "-p", "gcp:nosuch")
	if !strings.Contains(stderr, "-p gcp:nosuch matches none of the 3 GCP instances") {
		t.Errorf("-p gcp:nosuch:\n%s", stderr)
	}
	_, _, err := runHop(t, "ls", "-p", "gcp", "-r", "eu-west-1")
	if err == nil || !strings.Contains(err.Error(), "-r names AWS regions, which leaves out the GCP projects of -p gcp: drop -r") {
		t.Errorf("-p gcp -r eu-west-1: %v", err)
	}
}

func TestGCPLeftOutOfTheBackgroundRefresh(t *testing.T) {
	gcpEnv(t, "*")
	a, err := newApp(filters{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(a.olderThan(0, true), inventory.GCPPair()) {
		t.Fatal("GCP must be refreshed in the foreground")
	}
	a.background = true
	if slices.Contains(a.olderThan(0, true), inventory.GCPPair()) {
		t.Error("a background refresh runs gcloud")
	}
}

package inventory

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGcloud puts first on PATH a gcloud that prints a token, or fails as gcloud does
// when its login expired.
func fakeGcloud(t *testing.T, expired bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gcloud is a shell script")
	}
	script := "#!/bin/sh\necho fictional-token\n"
	if expired {
		script = "#!/bin/sh\n" +
			"echo 'ERROR: (gcloud.auth.print-access-token) There was a problem refreshing your current auth tokens: Reauthentication failed.' >&2\n" +
			"echo 'Please run:' >&2\nexit 1\n"
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

// gcpServer serves the Google APIs from handlers keyed by path and page token, on the
// loopback interface.
func gcpServer(t *testing.T, routes map[string]func(w http.ResponseWriter)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fictional-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.URL.Path
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			key += "#" + tok
		}
		h, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
			return
		}
		h(w)
	}))
	t.Cleanup(srv.Close)
	saved := GCPEndpoints
	GCPEndpoints.Compute, GCPEndpoints.ResourceManager = srv.URL, srv.URL
	t.Cleanup(func() { GCPEndpoints = saved })
}

func answer(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func gcpVM(id, name, zone, status string) string {
	return `{"id":"` + id + `","name":"` + name + `","zone":"https://www.googleapis.com/compute/v1/projects/p/zones/` + zone +
		`","status":"` + status + `","networkInterfaces":[{"networkIP":"10.0.0.` + id + `"}]}`
}

func TestFetchGCPFollowsEveryPage(t *testing.T) {
	fakeGcloud(t, false)
	gcpServer(t, map[string]func(http.ResponseWriter){
		"/v1/projects":    answer(200, `{"projects":[{"projectId":"shop-prod"},{"projectId":"other-team"}],"nextPageToken":"p2"}`),
		"/v1/projects#p2": answer(200, `{"projects":[{"projectId":"shop-test"},{"projectId":"shop-off"}]}`),
		"/compute/v1/projects/shop-prod/aggregated/instances":    answer(200, `{"items":{"zones/europe-west1-b":{"instances":[`+gcpVM("1", "web", "europe-west1-b", "RUNNING")+`]}},"nextPageToken":"i2"}`),
		"/compute/v1/projects/shop-prod/aggregated/instances#i2": answer(200, `{"items":{"zones/europe-west1-c":{"instances":[`+gcpVM("2", "batch", "europe-west1-c", "PROVISIONING")+`,`+gcpVM("3", "old", "europe-west1-c", "TERMINATED")+`]}}}`),
		"/compute/v1/projects/shop-test/aggregated/instances":    answer(200, `{"items":{"zones/us-east1-b":{"instances":[`+gcpVM("4", "web", "us-east1-b", "SUSPENDED")+`]}}}`),
		// Compute Engine off: skipped, not an error.
		"/compute/v1/projects/shop-off/aggregated/instances": answer(403, `{"error":{"code":403,"message":"Compute Engine API has not been used in project shop-off"}}`),
	})
	tg := FetchGCP(context.Background(), []string{"shop-*"})
	if tg.Err != "" {
		t.Fatal(tg.Err)
	}
	var got []string
	for _, in := range tg.Instances {
		got = append(got, in.Profile+" "+in.Region+" "+in.Name+" "+in.State)
	}
	want := []string{
		"gcp:shop-prod europe-west1-b web running",
		"gcp:shop-prod europe-west1-c batch pending",
		"gcp:shop-prod europe-west1-c old stopped",
		"gcp:shop-test us-east1-b web suspended",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("instances:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if tg.Key() != GCPPair().Key() || tg.FetchedAt.IsZero() {
		t.Errorf("target %s fetched at %v", tg.Key(), tg.FetchedAt)
	}
}

func TestFetchGCPFailures(t *testing.T) {
	cases := []struct {
		name    string
		expired bool
		routes  map[string]func(http.ResponseWriter)
		want    string
	}{
		{"gcloud login expired", true, nil,
			"gcloud auth print-access-token: There was a problem refreshing your current auth tokens: Reauthentication failed."},
		{"token refused", false, map[string]func(http.ResponseWriter){
			"/compute/v1/projects/shop-prod/aggregated/instances": answer(401, `{"error":{"code":401}}`),
		}, "project shop-prod: the gcloud token was refused: gcloud auth login"},
		{"server error", false, map[string]func(http.ResponseWriter){
			"/compute/v1/projects/shop-prod/aggregated/instances": answer(503, `{"error":{"code":503,"message":"backend unavailable"}}`),
		}, "project shop-prod: GCP API: HTTP 503: backend unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeGcloud(t, c.expired)
			routes := c.routes
			if routes == nil {
				routes = map[string]func(http.ResponseWriter){}
			}
			gcpServer(t, routes)
			tg := FetchGCP(context.Background(), []string{"shop-prod"})
			if tg.Err != c.want || len(tg.Instances) != 0 {
				t.Errorf("error %q, want %q", tg.Err, c.want)
			}
		})
	}
}

func TestLookupGCP(t *testing.T) {
	fakeGcloud(t, false)
	gcpServer(t, map[string]func(http.ResponseWriter){
		"/compute/v1/projects/shop-prod/zones/europe-west1-b/instances/web": answer(200, gcpVM("1", "web", "europe-west1-b", "TERMINATED")),
	})
	cached := Instance{ID: "1", Name: "web", Profile: GCPProfile("shop-prod"), Region: "europe-west1-b", State: "running"}
	in, found, err := LookupGCP(context.Background(), cached)
	if err != nil || !found || in.State != "stopped" || in.ID != "1" || in.Project() != "shop-prod" {
		t.Errorf("%+v, %v, %v", in, found, err)
	}
	cached.Name = "gone"
	if _, found, err := LookupGCP(context.Background(), cached); err != nil || found {
		t.Errorf("a missing instance: found %v, %v", found, err)
	}
}

func TestFetchGCPDoesNotSkipWhatItCouldNotRead(t *testing.T) {
	cases := map[string]string{
		// A quota answered with 403 is not a project to skip.
		`{"error":{"code":403,"message":"Quota exceeded","status":"PERMISSION_DENIED","errors":[{"reason":"rateLimitExceeded"}]}}`: "project shop-prod: GCP API: HTTP 403: Quota exceeded",
		// A zone that did not answer would look empty.
		`{"items":{},"unreachables":["zones/europe-west1-b"]}`: "project shop-prod: not reachable: zones/europe-west1-b",
	}
	for body, want := range cases {
		fakeGcloud(t, false)
		status := 200
		if strings.Contains(body, `"code":403`) {
			status = 403
		}
		gcpServer(t, map[string]func(http.ResponseWriter){
			"/compute/v1/projects/shop-prod/aggregated/instances": answer(status, body),
		})
		if tg := FetchGCP(context.Background(), []string{"shop-prod"}); tg.Err != want {
			t.Errorf("error %q, want %q", tg.Err, want)
		}
	}
}

func TestFetchGCPListsAProjectOnce(t *testing.T) {
	fakeGcloud(t, false)
	gcpServer(t, map[string]func(http.ResponseWriter){
		"/compute/v1/projects/shop-prod/aggregated/instances": answer(200, `{"items":{"zones/us-east1-b":{"instances":[`+gcpVM("1", "web", "us-east1-b", "RUNNING")+`]}}}`),
	})
	if tg := FetchGCP(context.Background(), []string{"shop-prod", "shop-prod"}); tg.Err != "" || len(tg.Instances) != 1 {
		t.Errorf("%+v", tg)
	}
}

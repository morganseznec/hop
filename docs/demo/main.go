// Command demo plays a fictional AWS fleet for the recording of hop's demo (see
// demo.tape): EC2 and SSM on the loopback interface, and a stand-in for
// session-manager-plugin that answers a few shell commands. Nothing in it reaches AWS.
//
//	demo serve DIR    writes the AWS files and DIR/env.sh, then serves for 5 minutes
//
// Built as DIR/bin/session-manager-plugin, the same binary is the plugin hop starts.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type host struct {
	name, env string
	stopped   bool
}

type account struct {
	profile, region string
	hosts           []host
}

// fleet is a consultant's view: three companies, nine accounts, six regions.
var fleet = []account{
	{"acme-prod", "eu-west-3", []host{
		{name: "shop-app", env: "prod"}, {name: "shop-api", env: "prod"}, {name: "shop-worker", env: "prod"},
		{name: "kafka-1", env: "prod"}, {name: "kafka-2", env: "prod"},
		{name: "grafana", env: "prod"}, {name: "tools", env: "prod"},
	}},
	{"acme-staging", "eu-west-3", []host{
		{name: "shop-app-staging", env: "staging"}, {name: "shop-api-staging", env: "staging"}, {name: "tools", env: "staging"},
	}},
	{"acme-data", "eu-west-1", []host{
		{name: "airflow", env: "prod"}, {name: "etl-runner", env: "prod"}, {name: "kafka-connect", env: "prod"},
	}},
	{"acme-shared", "eu-west-1", []host{
		{name: "jenkins"}, {name: "grafana"},
	}},
	{"globex-prod", "us-east-1", []host{
		{name: "web-1", env: "production"}, {name: "api-1", env: "production"}, {name: "grafana", env: "production"},
	}},
	{"globex-dev", "us-east-1", []host{
		{name: "api", env: "dev"}, {name: "tools", env: "dev"},
	}},
	{"globex-analytics", "us-west-2", []host{
		{name: "spark-master"}, {name: "spark-worker"},
	}},
	{"initech-prod", "eu-central-1", []host{
		{name: "tps-api", env: "prod"}, {name: "tps-worker", env: "prod"}, {name: "reports", env: "prod"},
		{name: "legacy-ftp", env: "prod", stopped: true},
	}},
	{"initech-test", "ap-southeast-2", []host{
		{name: "tps-api", env: "test"},
	}},
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "AWS_SSM_START_SESSION_RESPONSE" {
		plugin(os.Args[1:])
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: demo serve DIR")
		os.Exit(2)
	}
	if err := serve(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

// serve writes the AWS files of the fleet and the environment that points hop to
// this server, then answers for a while: the recording never outlives it.
func serve(dir string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	var config, credentials strings.Builder
	for i, a := range fleet {
		fmt.Fprintf(&config, "[profile %s]\nregion = %s\n\n", a.profile, a.region)
		fmt.Fprintf(&credentials, "[%s]\naws_access_key_id = %s\naws_secret_access_key = fictional\n\n", a.profile, accessKey(i))
	}
	env := fmt.Sprintf(`export HOME=%[1]s/home XDG_CONFIG_HOME=%[1]s/home/.config XDG_CACHE_HOME=%[1]s/home/.cache XDG_STATE_HOME=%[1]s/home/.local/state
export AWS_CONFIG_FILE=%[1]s/aws/config AWS_SHARED_CREDENTIALS_FILE=%[1]s/aws/credentials
export AWS_ENDPOINT_URL=http://%[2]s AWS_EC2_METADATA_DISABLED=true
unset AWS_PROFILE AWS_REGION AWS_DEFAULT_REGION AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
export PATH=%[1]s/bin:$PATH
`, dir, ln.Addr())
	for _, f := range []struct{ path, content string }{
		{"aws/config", config.String()},
		{"aws/credentials", credentials.String()},
		{"home/.keep", ""},
		{"env.sh", env}, // last: the tape waits for it
	} {
		p := filepath.Join(dir, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f.content), 0o600); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: http.HandlerFunc(handle), ReadHeaderTimeout: 10 * time.Second}
	time.AfterFunc(5*time.Minute, func() { _ = srv.Close() })
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func accessKey(i int) string { return fmt.Sprintf("AKIADEMO%012d", i+1) }

var credential = regexp.MustCompile(`Credential=([^/]+)/\d{8}/([^/]+)/`)

// caller returns the account a request was signed for, nil for an unknown key or a
// region the account does not use.
func caller(r *http.Request) *account {
	m := credential.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return nil
	}
	for i := range fleet {
		if accessKey(i) == m[1] && fleet[i].region == m[2] {
			return &fleet[i]
		}
	}
	return nil
}

func handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a := caller(r)
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		ssmCall(w, a, strings.TrimPrefix(target, "AmazonSSM."))
		return
	}
	form, _ := url.ParseQuery(string(body))
	w.Header().Set("Content-Type", "text/xml")
	if form.Get("Action") != "DescribeInstances" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidAction</Code></Error></Errors></Response>`)
		return
	}
	// The time a real account takes to answer, give or take.
	if a != nil {
		time.Sleep(time.Duration(200+hash(a.profile)%400) * time.Millisecond)
	}
	var items strings.Builder
	if a != nil {
		only := form.Get("InstanceId.1")
		for _, h := range a.hosts {
			id := instanceID(a, h)
			if only != "" && only != id {
				continue
			}
			state, code := "running", 16
			if h.stopped {
				state, code = "stopped", 80
			}
			fmt.Fprintf(&items, `<item><instanceId>%s</instanceId><instanceType>t3.medium</instanceType>`+
				`<instanceState><code>%d</code><name>%s</name></instanceState><privateIpAddress>%s</privateIpAddress>`+
				`<tagSet><item><key>Name</key><value>%s</value></item>`, id, code, state, privateIP(a, h), h.name)
			if h.env != "" {
				fmt.Fprintf(&items, `<item><key>Environment</key><value>%s</value></item>`, h.env)
			}
			items.WriteString(`</tagSet></item>`)
		}
	}
	fmt.Fprint(w, `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationSet><item><instancesSet>`+
		items.String()+`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
}

func ssmCall(w http.ResponseWriter, a *account, call string) {
	switch call {
	case "DescribeInstanceInformation":
		var list []map[string]string
		if a != nil {
			for _, h := range a.hosts {
				if !h.stopped {
					list = append(list, map[string]string{"InstanceId": instanceID(a, h), "PingStatus": "Online", "PlatformName": "Amazon Linux", "PlatformVersion": "2023"})
				}
			}
		}
		writeJSON(w, map[string]any{"InstanceInformationList": list})
	case "GetDocument":
		// Every account logs its sessions to CloudWatch.
		writeJSON(w, map[string]any{"Content": `{"inputs":{"cloudWatchLogGroupName":"/ssm-logs"}}`})
	case "StartSession":
		id := fmt.Sprintf("demo-%012x", time.Now().UnixNano()&0xffffffffffff)
		writeJSON(w, map[string]any{"SessionId": id, "TokenValue": "fictional", "StreamUrl": "wss://ssmmessages.example.invalid/" + id})
	case "TerminateSession":
		writeJSON(w, map[string]any{})
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"__type":"InvalidAction"}`)
	}
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprint(w, string(b))
}

func hash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func instanceID(a *account, h host) string {
	return fmt.Sprintf("i-0%016x", hash(a.profile+"/"+h.name))
}

func privateIP(a *account, h host) string {
	n := hash(a.profile + "/" + h.name)
	return fmt.Sprintf("10.%d.%d.%d", 10+hash(a.profile)%90, 1+n%3, 10+(n>>8)%200)
}

// plugin stands in for session-manager-plugin: hop calls it with the StartSession
// response, the region, the profile and the request. It plays a shell that knows a
// few commands, in the terminal as it is (the real plugin makes it raw).
func plugin(args []string) {
	var resp struct{ SessionId string }
	_ = json.Unmarshal([]byte(os.Getenv("AWS_SSM_START_SESSION_RESPONSE")), &resp)
	var req struct{ Target string }
	if len(args) > 4 {
		_ = json.Unmarshal([]byte(args[4]), &req)
	}
	hostname := "localhost"
	for i := range fleet {
		for _, h := range fleet[i].hosts {
			if instanceID(&fleet[i], h) == req.Target {
				hostname = "ip-" + strings.ReplaceAll(privateIP(&fleet[i], h), ".", "-") + "." + fleet[i].region + ".compute.internal"
				if fleet[i].region == "us-east-1" {
					hostname = strings.TrimSuffix(hostname, "us-east-1.compute.internal") + "ec2.internal"
				}
			}
		}
	}
	fmt.Printf("\nStarting session with SessionId: %s\n\n", resp.SessionId)
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("sh-5.2$ ")
		if !in.Scan() {
			break
		}
		cmd := strings.TrimSpace(in.Text())
		switch {
		case cmd == "":
		case cmd == "exit":
			fmt.Printf("\n\nExiting session with sessionId: %s.\n\n", resp.SessionId)
			return
		case cmd == "hostname":
			fmt.Println(hostname)
		case cmd == "whoami":
			fmt.Println("ssm-user")
		case cmd == "uptime":
			fmt.Println(" 10:42:17 up 23 days,  4:11,  1 user,  load average: 0.08, 0.05, 0.01")
		case strings.HasPrefix(cmd, "systemctl is-active"):
			fmt.Println("active")
		default:
			fmt.Printf("sh: %s: command not found\n", strings.Fields(cmd)[0])
		}
	}
	fmt.Println()
}

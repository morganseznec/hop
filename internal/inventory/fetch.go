package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

// AWSConfig loads a profile in the given region. The region is ALWAYS the one of the
// pair being queried: an account queried outside its region looks empty. A copy of the
// configuration with another Region set queries that region with the same
// credentials cache (see FetchAll).
//
// preferSSO skips the credentials file, so that a profile defined both ways goes
// through SSO, as it does with the aws CLI (see awsconf.Profile.PreferSSO).
func AWSConfig(ctx context.Context, profile, region string, preferSSO bool) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithSharedConfigProfile(profile),
		awsconfig.WithRegion(region),
		awsconfig.WithRetryMaxAttempts(2),
	}
	if preferSSO {
		// An empty list, not nil: nil would mean "default files".
		opts = append(opts, awsconfig.WithSharedCredentialsFiles([]string{}))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// Fetch queries one profile/region pair: EC2 instances, SSM agent state and
// transcript settings. It never returns an error: the Target carries it.
func Fetch(ctx context.Context, p Pair) *Target {
	cfg, err := AWSConfig(ctx, p.Profile, p.Region, p.PreferSSO)
	if err != nil {
		return failed(p, time.Now(), err)
	}
	return fetch(ctx, cfg, p)
}

// failed is the Target of a pair whose query, started at the given time, failed.
func failed(p Pair, at time.Time, err error) *Target {
	return &Target{Profile: p.Profile, Region: p.Region, Err: Redact(err.Error()), ErrKind: Classify(err), ErrAt: at}
}

// fetch queries a pair with a configuration loaded for its profile and set to its
// region. The EC2, SSM agent and transcript calls run in parallel: on an account with
// thousands of nodes, the agent pages can use up the time allowed, and must not keep
// the transcript settings from being read. The times recorded are those of the start
// of the query, so that the cache orders them against an Invalidate made meanwhile by
// a login (see Save).
func fetch(ctx context.Context, cfg aws.Config, p Pair) *Target {
	start := time.Now()
	// Retrieve the credentials first: an expired SSO token fails here, quickly and with
	// a typed error, instead of three calls that each fail in their own way.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return failed(p, start, err)
	}

	sc := ssm.NewFromConfig(cfg)
	var (
		wg                 sync.WaitGroup
		instances, managed []Instance
		agents             map[string]agentInfo
		ec2Err, agentsErr  error
		tr                 Transcript
	)
	wg.Go(func() { instances, ec2Err = describeEC2(ctx, ec2.NewFromConfig(cfg), p.Profile, p.Region) })
	wg.Go(func() { agents, managed, agentsErr = describeAgents(ctx, sc, p.Profile, p.Region) })
	wg.Go(func() { tr = transcript(ctx, sc) })
	wg.Wait()
	if ec2Err != nil {
		t := failed(p, start, ec2Err)
		t.Transcript = tr
		return t
	}
	t := &Target{Profile: p.Profile, Region: p.Region, FetchedAt: start, Transcript: tr, AgentsKnown: agentsErr == nil}
	if agentsErr == nil {
		for i := range instances {
			if a, ok := agents[instances[i].ID]; ok {
				copyAgent(&instances[i], Instance{Agent: a.Agent, LastPing: a.LastPing, Platform: a.Platform})
			}
		}
		instances = append(instances, managed...)
	}
	t.Instances = instances
	return t
}

// Lookup reads one instance again from AWS, with cfg loaded for its profile and
// region: its EC2 state, name and address, and its SSM agent. found is false when the
// instance no longer exists. It starts from the cached copy, so the agent stays as it
// was when SSM refuses the call. A hybrid node (mi-) exists only in SSM: there, the
// SSM call is the lookup.
func Lookup(ctx context.Context, cfg aws.Config, cached Instance) (in Instance, found bool, err error) {
	byID := ssmtypes.InstanceInformationStringFilter{Key: aws.String("InstanceIds"), Values: []string{cached.ID}}
	var (
		wg                sync.WaitGroup
		agents            map[string]agentInfo
		managed           []Instance
		agentsErr, ec2Err error
	)
	wg.Go(func() {
		agents, managed, agentsErr = describeAgents(ctx, ssm.NewFromConfig(cfg), cached.Profile, cached.Region, byID)
	})
	if !cached.Hybrid() {
		wg.Go(func() { in, found, ec2Err = describeOne(ctx, ec2.NewFromConfig(cfg), cached) })
	}
	wg.Wait()

	if cached.Hybrid() {
		if agentsErr != nil {
			return cached, false, agentsErr
		}
		for _, m := range managed {
			if m.ID == cached.ID {
				m.AlsoVia = cached.AlsoVia
				return m, true, nil
			}
		}
		return cached, false, nil
	}
	if ec2Err != nil || !found {
		return cached, false, ec2Err
	}
	agent := cached
	if agentsErr == nil {
		a := agents[in.ID] // zero when no agent is registered
		agent = Instance{Agent: a.Agent, LastPing: a.LastPing, Platform: a.Platform}
	}
	copyAgent(&in, agent)
	in.AlsoVia = cached.AlsoVia
	return in, true, nil
}

// describeOne reads one EC2 instance by ID, whatever its state: a terminated instance
// is still described for a while, as terminated.
func describeOne(ctx context.Context, c *ec2.Client, cached Instance) (Instance, bool, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{cached.ID}})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "InvalidInstanceID.NotFound" {
			return Instance{}, false, nil
		}
		return Instance{}, false, err
	}
	for _, r := range out.Reservations {
		for _, in := range r.Instances {
			if aws.ToString(in.InstanceId) == cached.ID {
				return toInstance(in, cached.Profile, cached.Region), true, nil
			}
		}
	}
	return Instance{}, false, nil
}

func describeEC2(ctx context.Context, c *ec2.Client, profile, region string) ([]Instance, error) {
	var out []Instance
	p := ec2.NewDescribeInstancesPaginator(c, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []string{"pending", "running", "stopping", "stopped"},
		}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			for _, in := range r.Instances {
				out = append(out, toInstance(in, profile, region))
			}
		}
	}
	return out, nil
}

// toInstance turns an EC2 instance into an inventory entry, tags cleaned.
func toInstance(in ec2types.Instance, profile, region string) Instance {
	tags := map[string]string{}
	for _, tg := range in.Tags {
		tags[clean(aws.ToString(tg.Key))] = clean(aws.ToString(tg.Value))
	}
	id := aws.ToString(in.InstanceId)
	// An instance without a Name tag is still reachable: show its ID instead of hiding
	// it.
	name := tags["Name"]
	if name == "" {
		name = id
	}
	state := ""
	if in.State != nil {
		state = string(in.State.Name)
	}
	return Instance{
		ID:        id,
		Name:      name,
		Profile:   profile,
		Region:    region,
		PrivateIP: aws.ToString(in.PrivateIpAddress),
		State:     state,
		Env:       DeriveEnv(tags, name, profile),
		Platform:  clean(aws.ToString(in.PlatformDetails)),
	}
}

// clean removes control characters from text that hop prints but does not control:
// tags, and what the agent of a managed server reports. Anyone allowed to tag an
// instance could otherwise send escape sequences (or line breaks) to the terminal of
// whoever lists it, and rewrite hop's output. Bidirectional overrides go too, since
// they reorder what is displayed.
// Clean drops control and bidi characters from text read from AWS (tags, names),
// which ends up in the terminal.
func Clean(s string) string { return clean(s) }

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, s)
}

type agentInfo struct {
	Agent    string
	LastPing time.Time
	Platform string
}

// agentPage is the largest page DescribeInstanceInformation returns. With the default
// of 10, an account with 2,000 nodes takes 200 calls in a row: more than the time
// allowed for a pair.
const agentPage = 50

// describeAgents returns the agent state of each EC2 instance, plus the hybrid servers
// (mi-...) registered in SSM, which do not exist on the EC2 side. filters narrows the
// nodes described.
func describeAgents(ctx context.Context, c *ssm.Client, profile, region string, filters ...ssmtypes.InstanceInformationStringFilter) (map[string]agentInfo, []Instance, error) {
	agents := map[string]agentInfo{}
	var managed []Instance
	p := ssm.NewDescribeInstanceInformationPaginator(c, &ssm.DescribeInstanceInformationInput{
		MaxResults: aws.Int32(agentPage),
		Filters:    filters,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, ii := range page.InstanceInformationList {
			id := aws.ToString(ii.InstanceId)
			// The agent of a managed server reports its own name, platform and address.
			a := agentInfo{Agent: string(ii.PingStatus), LastPing: aws.ToTime(ii.LastPingDateTime), Platform: clean(aws.ToString(ii.PlatformName))}
			agents[id] = a
			if strings.HasPrefix(id, "mi-") {
				name := clean(aws.ToString(ii.Name))
				if name == "" {
					name = clean(aws.ToString(ii.ComputerName))
				}
				if name == "" {
					name = id
				}
				managed = append(managed, Instance{
					ID: id, Name: name, Profile: profile, Region: region,
					PrivateIP: clean(aws.ToString(ii.IPAddress)), State: "running",
					Env: DeriveEnv(nil, name, profile), Platform: a.Platform,
					Agent: a.Agent, LastPing: a.LastPing,
				})
			}
		}
	}
	return agents, managed, nil
}

// transcript reads the Session Manager preferences. They live in the
// SSM-SessionManagerRunShell document, one per account and per region: that is why an
// account recorded in eu-west-1 is not necessarily recorded in us-east-1.
func transcript(ctx context.Context, c *ssm.Client) Transcript {
	out, err := c.GetDocument(ctx, &ssm.GetDocumentInput{Name: aws.String("SSM-SessionManagerRunShell")})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "InvalidDocument" {
			// No document: Session Manager runs with its defaults, without any
			// logging.
			return Transcript{Checked: true}
		}
		return Transcript{Err: Redact(err.Error())}
	}
	return ParseSessionPrefs(aws.ToString(out.Content))
}

func ParseSessionPrefs(content string) Transcript {
	var doc struct {
		Inputs struct {
			S3BucketName                string `json:"s3BucketName"`
			S3EncryptionEnabled         bool   `json:"s3EncryptionEnabled"`
			CloudWatchLogGroupName      string `json:"cloudWatchLogGroupName"`
			CloudWatchEncryptionEnabled bool   `json:"cloudWatchEncryptionEnabled"`
			KmsKeyID                    string `json:"kmsKeyId"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return Transcript{Err: "unreadable document: " + err.Error()}
	}
	in := doc.Inputs
	t := Transcript{Checked: true, LogGroup: clean(in.CloudWatchLogGroupName), Bucket: clean(in.S3BucketName)}
	t.Enabled = t.LogGroup != "" || t.Bucket != ""
	t.Encrypted = in.KmsKeyID != "" || (t.LogGroup != "" && in.CloudWatchEncryptionEnabled) || (t.Bucket != "" && in.S3EncryptionEnabled)
	return t
}

// Pair is a profile/region pair to refresh.
type Pair struct {
	Profile, Region string
	PreferSSO       bool
}

// FetchAll queries the pairs in parallel, sixteen at a time. Each profile is loaded
// once and its regions share that configuration, credentials cache included: an SSO
// role or an assumed role is resolved once per profile, not once per region. progress
// is called each time a pair completes.
func FetchAll(ctx context.Context, pairs []Pair, timeout time.Duration, progress func(done, total int)) []*Target {
	out := make([]*Target, len(pairs))
	profiles := map[profileKey]*profileConfig{}
	for _, p := range pairs {
		if k := (profileKey{p.Profile, p.PreferSSO}); profiles[k] == nil {
			profiles[k] = &profileConfig{}
		}
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for i, p := range pairs {
		pc := profiles[profileKey{p.Profile, p.PreferSSO}]
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out[i] = pc.fetch(cctx, p)
			mu.Lock()
			done++
			if progress != nil {
				progress(done, len(pairs))
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

type profileKey struct {
	profile   string
	preferSSO bool
}

// profileConfig is the configuration of a profile, loaded by the first of its pairs to
// run.
type profileConfig struct {
	once sync.Once
	cfg  aws.Config
	err  error
}

// fetch queries a pair of the profile, with a copy of its configuration set to the
// pair's region.
func (pc *profileConfig) fetch(ctx context.Context, p Pair) *Target {
	pc.once.Do(func() { pc.cfg, pc.err = AWSConfig(ctx, p.Profile, p.Region, p.PreferSSO) })
	if pc.err != nil {
		return failed(p, time.Now(), pc.err)
	}
	cfg := pc.cfg.Copy()
	cfg.Region = p.Region
	return fetch(ctx, cfg, p)
}

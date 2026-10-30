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
	"github.com/aws/smithy-go"
)

// AWSConfig loads a profile in the given region. The region is ALWAYS the one of the
// pair being queried: an account queried outside its region looks empty.
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
	profile, region := p.Profile, p.Region
	t := &Target{Profile: profile, Region: region}
	fail := func(err error) *Target {
		t.Err = err.Error()
		t.ErrKind = Classify(err)
		t.ErrAt = time.Now()
		return t
	}
	cfg, err := AWSConfig(ctx, profile, region, p.PreferSSO)
	if err != nil {
		return fail(err)
	}
	// Retrieve the credentials first: an expired SSO token fails here, quickly and with
	// a typed error, instead of three calls that each fail in their own way.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return fail(err)
	}

	instances, err := describeEC2(ctx, ec2.NewFromConfig(cfg), profile, region)
	if err != nil {
		return fail(err)
	}
	sc := ssm.NewFromConfig(cfg)
	agents, managed, err := describeAgents(ctx, sc, profile, region)
	t.AgentsKnown = err == nil
	if err == nil {
		for i := range instances {
			if a, ok := agents[instances[i].ID]; ok {
				instances[i].Agent = a.Agent
				instances[i].LastPing = a.LastPing
				if a.Platform != "" {
					instances[i].Platform = a.Platform
				}
			}
		}
		instances = append(instances, managed...)
	}
	t.Instances = instances
	t.Transcript = transcript(ctx, sc)
	t.FetchedAt = time.Now()
	return t
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
				tags := map[string]string{}
				for _, tg := range in.Tags {
					tags[clean(aws.ToString(tg.Key))] = clean(aws.ToString(tg.Value))
				}
				id := aws.ToString(in.InstanceId)
				// An instance without a Name tag is still reachable: show its ID instead
				// of hiding it.
				name := tags["Name"]
				if name == "" {
					name = id
				}
				state := ""
				if in.State != nil {
					state = string(in.State.Name)
				}
				out = append(out, Instance{
					ID:        id,
					Name:      name,
					Profile:   profile,
					Region:    region,
					PrivateIP: aws.ToString(in.PrivateIpAddress),
					State:     state,
					Env:       DeriveEnv(tags, name, profile),
					Platform:  clean(aws.ToString(in.PlatformDetails)),
				})
			}
		}
	}
	return out, nil
}

// clean removes control characters from text that hop prints but does not control:
// tags, and what the agent of a managed server reports. Anyone allowed to tag an
// instance could otherwise send escape sequences (or line breaks) to the terminal of
// whoever lists it, and rewrite hop's output. Bidirectional overrides go too, since
// they reorder what is displayed.
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

// describeAgents returns the agent state of each EC2 instance, plus the hybrid servers
// (mi-...) registered in SSM, which do not exist on the EC2 side.
func describeAgents(ctx context.Context, c *ssm.Client, profile, region string) (map[string]agentInfo, []Instance, error) {
	agents := map[string]agentInfo{}
	var managed []Instance
	p := ssm.NewDescribeInstanceInformationPaginator(c, &ssm.DescribeInstanceInformationInput{})
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
		return Transcript{Err: err.Error()}
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

// FetchAll queries the pairs in parallel, eight at a time. progress is called each
// time a pair completes.
func FetchAll(ctx context.Context, pairs []Pair, timeout time.Duration, progress func(done, total int)) []*Target {
	out := make([]*Target, len(pairs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for i, p := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out[i] = Fetch(cctx, p)
			mu.Lock()
			done++
			if progress != nil {
				progress(done, len(pairs))
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

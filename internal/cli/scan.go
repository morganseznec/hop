package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"

	"github.com/morganseznec/hop/internal/inventory"
)

type scanHit struct {
	profile, region string
	names           []string
}

func newScan(f *filters) *cobra.Command {
	var write bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Find instances outside the watched regions and offer to add them",
		Long: `For each profile, queries every region enabled on the account. An instance found
outside the profile's region (say, a test environment in us-east-1) is suggested as
extra_regions. --write adds it to ~/.config/hop/config.yaml.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(*f)
			if err != nil {
				return err
			}
			hits, failed := a.scan(cmd.Context())
			for _, fl := range failed {
				warn("%s", fl)
			}
			if len(hits) == 0 {
				info("no instances outside the regions already watched")
				return nil
			}
			info("instances found outside the watched regions:")
			for _, h := range hits {
				names := h.names
				if len(names) > 4 {
					names = append(names[:4:4], fmt.Sprintf("+%d", len(h.names)-4))
				}
				fmt.Fprintf(os.Stderr, "  %-24s %-14s %s\n", h.profile, h.region, strings.Join(names, ", "))
			}
			if !write {
				fmt.Fprintln(os.Stderr, "\nsuggested for ~/.config/hop/config.yaml:\n\nextra_regions:")
				for _, p := range groupHits(hits) {
					fmt.Fprintf(os.Stderr, "  %s: [%s]\n", p.profile, strings.Join(p.regions, ", "))
				}
				fmt.Fprintln(os.Stderr)
				hint("hop scan --write to apply it")
				return nil
			}
			if a.cfg.ExtraRegions == nil {
				a.cfg.ExtraRegions = map[string][]string{}
			}
			for _, h := range hits {
				if !slices.Contains(a.cfg.ExtraRegions[h.profile], h.region) {
					a.cfg.ExtraRegions[h.profile] = append(a.cfg.ExtraRegions[h.profile], h.region)
				}
			}
			if err := a.cfg.Save(); err != nil {
				return err
			}
			info("regions added, previous file kept as .bak")
			return nil
		},
	}
	cmd.Flags().BoolVar(&write, "write", false, "add the regions found to the config file")
	return cmd
}

type profileRegions struct {
	profile string
	regions []string
}

func groupHits(hits []scanHit) []profileRegions {
	var out []profileRegions
	idx := map[string]int{}
	for _, h := range hits {
		i, ok := idx[h.profile]
		if !ok {
			out = append(out, profileRegions{profile: h.profile})
			i = len(out) - 1
			idx[h.profile] = i
		}
		out[i].regions = append(out[i].regions, h.region)
	}
	return out
}

func (a *app) scan(ctx context.Context) ([]scanHit, []string) {
	var (
		mu     sync.Mutex
		hits   []scanHit
		failed []string
		wg     sync.WaitGroup
		sem    = make(chan struct{}, 16)
		total  int
		done   int
	)
	tick := func() {
		done++
		if stderrTTY {
			fmt.Fprintf(os.Stderr, "\r\033[Khop: scan %d/%d", done, total)
		}
	}
	profiles := a.selectedProfiles()
	total = len(profiles)
	for _, p := range profiles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			known := a.cfg.Regions(p.Name, p.Region, a.files.DefaultRegion)
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			cfg, err := inventory.AWSConfig(cctx, p.Name, known[0], p.PreferSSO())
			var regions []string
			if err == nil {
				regions, err = enabledRegions(cctx, cfg)
			}
			cancel()
			<-sem
			mu.Lock()
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s not scanned: %s", p.Name, inventory.Classify(err).Explain()))
				tick()
				mu.Unlock()
				return
			}
			var todo []string
			for _, r := range regions {
				if !slices.Contains(known, r) {
					todo = append(todo, r)
				}
			}
			total += len(todo)
			tick()
			mu.Unlock()
			var inner sync.WaitGroup
			for _, r := range todo {
				inner.Add(1)
				go func() {
					defer inner.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
					defer cancel()
					rc := cfg.Copy()
					rc.Region = r
					names, _ := instanceNamesIn(rctx, rc)
					mu.Lock()
					if len(names) > 0 {
						hits = append(hits, scanHit{p.Name, r, names})
					}
					tick()
					mu.Unlock()
				}()
			}
			inner.Wait()
		}()
	}
	wg.Wait()
	if stderrTTY {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].profile != hits[j].profile {
			return hits[i].profile < hits[j].profile
		}
		return hits[i].region < hits[j].region
	})
	sort.Strings(failed)
	return hits, failed
}

func enabledRegions(ctx context.Context, cfg aws.Config) ([]string, error) {
	out, err := ec2.NewFromConfig(cfg).DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, err
	}
	var regions []string
	for _, r := range out.Regions {
		regions = append(regions, aws.ToString(r.RegionName))
	}
	return regions, nil
}

func instanceNamesIn(ctx context.Context, cfg aws.Config) ([]string, error) {
	out, err := ec2.NewFromConfig(cfg).DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}}},
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range out.Reservations {
		for _, in := range r.Instances {
			name := aws.ToString(in.InstanceId)
			for _, t := range in.Tags {
				if aws.ToString(t.Key) == "Name" && aws.ToString(t.Value) != "" {
					name = aws.ToString(t.Value)
				}
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

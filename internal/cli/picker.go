package cli

import (
	"errors"
	"fmt"
	"strings"

	fuzzyfinder "github.com/ktr0731/go-fuzzyfinder"
	"github.com/mattn/go-runewidth"

	"github.com/morganseznec/hop/internal/inventory"
)

var errAborted = errors.New("canceled")

// pick opens the fuzzy picker. Each line holds name, profile, client label, region and
// environment: typing "shop prod app" filters on all of them at once.
func (a *app) pick(items []inventory.Instance, header string) (inventory.Instance, error) {
	if !stdinTTY || !stderrTTY {
		return inventory.Instance{}, severalMatch(items)
	}
	wName, wProf := 0, 0
	for _, in := range items {
		wName = max(wName, runewidth.StringWidth(in.Name))
		wProf = max(wProf, runewidth.StringWidth(a.profileLabel(in.Profile)))
	}
	line := func(i int) string {
		in := items[i]
		mark := "·"
		if in.Online() {
			mark = "●"
		}
		s := fmt.Sprintf("%s %s  %s  %-14s %s",
			mark,
			runewidth.FillRight(in.Name, wName),
			runewidth.FillRight(a.profileLabel(in.Profile), wProf),
			in.Region, in.Env)
		if !in.Running() && in.State != "" {
			s += "  (" + in.State + ")"
		}
		return s
	}
	opts := []fuzzyfinder.Option{
		fuzzyfinder.WithPromptString("hop> "),
		fuzzyfinder.WithPreviewWindow(func(i, w, h int) string {
			if i < 0 {
				return ""
			}
			return a.preview(items[i])
		}),
	}
	if header == "" {
		header = fmt.Sprintf("%d instances · ● agent online · Enter to connect, Esc to cancel", len(items))
	}
	opts = append(opts, fuzzyfinder.WithHeader(header))
	i, err := fuzzyfinder.Find(items, line, opts...)
	if errors.Is(err, fuzzyfinder.ErrAbort) {
		return inventory.Instance{}, errAborted
	}
	if err != nil {
		return inventory.Instance{}, err
	}
	return items[i], nil
}

func (a *app) profileLabel(profile string) string {
	if l := a.cfg.Label(profile); l != "" {
		return profile + " (" + l + ")"
	}
	return profile
}

func (a *app) preview(in inventory.Instance) string {
	var b strings.Builder
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%-13s %s\n", k, v)
		}
	}
	row("name", in.Name)
	if !in.Running() {
		row("state", in.State)
	}
	row("instance", in.ID)
	row("profile", a.profileLabel(in.Profile))
	row("region", in.Region)
	row("env", in.Env)
	row("private IP", in.PrivateIP)
	row("platform", in.Platform)
	if in.GCP() {
		row("connection", "IAP, with gcloud compute ssh")
		row("logging", "no: GCP does not record shell sessions")
	} else {
		agent := in.Agent
		switch {
		case agent == "" && a.target(in) != nil && a.target(in).AgentsKnown:
			agent = "none"
		case agent == "":
			agent = "unknown"
		case !in.LastPing.IsZero():
			agent += ", " + ago(in.LastPing)
		}
		row("SSM agent", agent)
		if t := a.target(in); t != nil {
			row("logging", transcriptText(t.Transcript))
		}
	}
	if t := a.target(in); t != nil {
		row("inventory", ago(t.FetchedAt))
	}
	if len(in.AlsoVia) > 0 {
		row("also via", strings.Join(in.AlsoVia, ", "))
	}
	if t, ok := a.recents[in.Key()]; ok {
		row("last used", ago(t))
	}
	return b.String()
}

func transcriptText(t inventory.Transcript) string {
	switch {
	case !t.Checked:
		return "not verified"
	case t.Enabled:
		return "yes, to " + t.Destination()
	}
	return "NO (the SSM-SessionManagerRunShell document has no destination)"
}

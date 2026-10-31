package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
)

// The forms of Compute Engine names. The names come from the inventory: they are
// checked before they go on gcloud's command line, where one starting with - would be
// read as an option.
var (
	gcpName    = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	gcpZone    = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
	gcpProject = regexp.MustCompile(`^([a-z0-9][-a-z0-9.]*[a-z0-9]:)?[a-z][-a-z0-9]{4,28}[a-z0-9]$`)
)

// GCPArgs are the gcloud arguments that reach a Compute Engine instance through IAP,
// after the gcloud command (compute ssh, compute scp...).
func GCPArgs(project, zone string) []string {
	return []string{"--project=" + project, "--zone=" + zone, "--tunnel-through-iap"}
}

// StartGCP opens a shell on a Compute Engine instance with gcloud compute ssh, through
// IAP: the instance needs no external IP. gcloud manages the SSH key, through OS Login
// or the project metadata.
func StartGCP(project, zone, name string) error {
	if !gcpProject.MatchString(project) || !gcpZone.MatchString(zone) || !gcpName.MatchString(name) {
		return fmt.Errorf("invalid GCP instance %q (project %q, zone %q)", name, project, zone)
	}
	gcloud, err := exec.LookPath("gcloud")
	if err != nil {
		return errors.New("gcloud not found: install the Google Cloud CLI (https://cloud.google.com/sdk/docs/install)")
	}
	cmd := exec.Command(gcloud, append([]string{"compute", "ssh", name}, GCPArgs(project, zone)...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C and friends belong to the remote shell.
	restore := ignoreSignals()
	defer restore()
	return cmd.Run()
}

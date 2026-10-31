// Package session opens Session Manager sessions and runs commands.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"
)

// Request describes a session: a shell, recorded per the account's preferences, if
// Document is empty, port forwarding otherwise.
type Request struct {
	Profile    string
	Region     string
	InstanceID string
	Document   string
	Parameters map[string][]string
	// PreferSSO: the plugin reads the profile again to encrypt the stream (KMS). It must
	// then ignore the keys of the credentials file as well.
	PreferSSO bool
}

// PluginLocations are where the plugin installers put session-manager-plugin. PluginPath
// looks there when it is not on PATH: editors and other desktop apps start ssh, and so
// hop ssh-proxy, with a PATH that often leaves out /opt/homebrew/bin or /usr/local/bin.
var PluginLocations = pluginLocations()

// PluginPath finds session-manager-plugin on PATH, then in PluginLocations.
func PluginPath() (string, error) {
	if p, err := exec.LookPath("session-manager-plugin"); err == nil {
		return p, nil
	}
	for _, p := range PluginLocations {
		if p, err := exec.LookPath(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("session-manager-plugin not found: " + pluginInstallHint)
}

// Start opens the session and hands over to the plugin, like `aws ssm start-session`:
//   - the StartSession response goes through the AWS_SSM_START_SESSION_RESPONSE
//     variable, so the session token does not show up in `ps`;
//   - Ctrl-C, Ctrl-\ and Ctrl-Z are ignored here and left to the plugin, which forwards
//     them to the remote shell instead of ending the session.
func Start(ctx context.Context, cfg aws.Config, req Request) error {
	plugin, err := PluginPath()
	if err != nil {
		return err
	}
	in := &ssm.StartSessionInput{Target: aws.String(req.InstanceID)}
	params := map[string]any{"Target": req.InstanceID}
	if req.Document != "" {
		in.DocumentName = aws.String(req.Document)
		in.Parameters = req.Parameters
		params["DocumentName"] = req.Document
		params["Parameters"] = req.Parameters
	}
	client := ssm.NewFromConfig(cfg)
	out, err := client.StartSession(ctx, in)
	if err != nil {
		return explainStart(err, req.InstanceID)
	}

	resp, _ := json.Marshal(map[string]string{
		"SessionId":  aws.ToString(out.SessionId),
		"TokenValue": aws.ToString(out.TokenValue),
		"StreamUrl":  aws.ToString(out.StreamUrl),
	})
	reqJSON, _ := json.Marshal(params)
	endpoint := pluginEndpoint(ctx, client.Options())

	cmd := exec.Command(plugin, "AWS_SSM_START_SESSION_RESPONSE", req.Region, "StartSession", req.Profile, string(reqJSON), endpoint)
	cmd.Env = append(os.Environ(), "AWS_SSM_START_SESSION_RESPONSE="+string(resp))
	if req.PreferSSO {
		// The null device reads as an empty file: no keys, so SSO applies.
		cmd.Env = append(cmd.Env, "AWS_SHARED_CREDENTIALS_FILE="+os.DevNull)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// Ctrl-C and friends belong to the remote shell: the plugin forwards them.
	restore := ignoreSignals()
	defer restore()
	if err := cmd.Start(); err != nil {
		// Without the plugin, the session opened on the agent side would stay active
		// until it expires: terminate it.
		terminate(ctx, client, aws.ToString(out.SessionId))
		return err
	}
	return cmd.Wait()
}

// terminate closes a session, best effort, with a context of its own (the caller's may
// already be canceled) and bounded like the cancellation of a command.
func terminate(ctx context.Context, client *ssm.Client, sessionID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	_, _ = client.TerminateSession(ctx, &ssm.TerminateSessionInput{SessionId: aws.String(sessionID)})
}

// pluginEndpoint returns the SSM endpoint the SDK resolves for hop's own calls, so that
// the plugin follows AWS_ENDPOINT_URL_SSM, the FIPS and dual-stack settings and the
// China partition too.
func pluginEndpoint(ctx context.Context, o ssm.Options) string {
	if o.BaseEndpoint != nil {
		return *o.BaseEndpoint
	}
	ep, err := ssm.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, ssm.EndpointParameters{
		Region:       aws.String(o.Region),
		UseFIPS:      aws.Bool(o.EndpointOptions.UseFIPSEndpoint == aws.FIPSEndpointStateEnabled),
		UseDualStack: aws.Bool(o.EndpointOptions.UseDualStackEndpoint == aws.DualStackEndpointStateEnabled),
	})
	if err != nil {
		return fmt.Sprintf("https://ssm.%s.amazonaws.com", o.Region)
	}
	return ep.URI.String()
}

// explainStart turns the usual StartSession errors into a message that says what to
// check on the instance or in IAM.
func explainStart(err error, id string) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "TargetNotConnected":
			return fmt.Errorf("the SSM agent on %s is not connected: the instance is stopped, the agent is missing, or it cannot reach the SSM endpoints", id)
		case "AccessDeniedException":
			return fmt.Errorf("IAM denied access to start a session on %s: %s", id, api.ErrorMessage())
		case "InvalidDocument":
			return fmt.Errorf("session document not found in this region: %s", api.ErrorMessage())
		}
	}
	return err
}

// Package awsauth builds the AWS SDK configuration for the ecs and lambda plugins: target
// region, credentials from manent secrets or from the SDK default chain (environment
// variables, IRSA role, ECS task or EC2 instance role) and an optional assume role.
package awsauth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Descriptor parameters (deploy.params) shared by the AWS plugins.
const (
	ParamAccessKeySecret    = "access_key_id_secret"     // secret with the access key ID
	ParamSecretKeySecret    = "secret_access_key_secret" // secret with the secret access key
	ParamSessionTokenSecret = "session_token_secret"     // secret with the session token (temporary credentials)
	ParamRoleARN            = "role_arn"                 // role to assume with STS
	ParamExternalID         = "external_id"              // external ID required by the role
)

var (
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	roleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]+$`)
	sessionRe = regexp.MustCompile(`[^\w+=,.@-]`)
)

// ValidRegion reports whether s is an AWS region name (e.g. eu-west-1).
func ValidRegion(s string) bool { return regionRe.MatchString(s) }

// Validate checks the credential parameters.
func Validate(params map[string]string) error {
	var errs []error
	if (params[ParamAccessKeySecret] == "") != (params[ParamSecretKeySecret] == "") {
		errs = append(errs, fmt.Errorf("%q and %q must be set together", ParamAccessKeySecret, ParamSecretKeySecret))
	}
	if params[ParamSessionTokenSecret] != "" && params[ParamAccessKeySecret] == "" {
		errs = append(errs, fmt.Errorf("%q requires %q and %q", ParamSessionTokenSecret, ParamAccessKeySecret, ParamSecretKeySecret))
	}
	if r := params[ParamRoleARN]; r != "" && !roleARNRe.MatchString(r) {
		errs = append(errs, fmt.Errorf("invalid %s %q: expected arn:aws:iam::<account>:role/<name>", ParamRoleARN, r))
	}
	if params[ParamExternalID] != "" && params[ParamRoleARN] == "" {
		errs = append(errs, fmt.Errorf("%q requires %q", ParamExternalID, ParamRoleARN))
	}
	return errors.Join(errs...)
}

// Config returns the SDK configuration for the region. Static credentials are read from the
// secrets only here, at use time, and are never logged.
func Config(ctx context.Context, region string, req *plugin.Request) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if req.Params[ParamAccessKeySecret] != "" {
		var id, secret, token string
		var err error
		read := func(param string, dst *string) {
			if name := req.Params[param]; name != "" && err == nil {
				*dst, err = req.Secrets.Secret(ctx, name)
				*dst = strings.TrimSpace(*dst)
			}
		}
		read(ParamAccessKeySecret, &id)
		read(ParamSecretKeySecret, &secret)
		read(ParamSessionTokenSecret, &token)
		if err != nil {
			return aws.Config{}, err
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, secret, token)))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("AWS configuration: %w", err)
	}
	if role := req.Params[ParamRoleARN]; role != "" {
		ext := req.Params[ParamExternalID]
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), role, func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = SessionName(req.DeploymentID)
			if ext != "" {
				o.ExternalID = aws.String(ext)
			}
		}))
	}
	return cfg, nil
}

// SessionName is the STS session name: it carries the deploy ID, so CloudTrail ties the calls
// to the deploy that made them.
func SessionName(deploymentID string) string {
	s := "manent-" + sessionRe.ReplaceAllString(deploymentID, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// Endpoint returns the alternative endpoint from the plugin configuration (e.g. LocalStack),
// nil if not set.
func Endpoint(cfg map[string]any) (*string, error) {
	raw, ok := cfg["endpoint"]
	if !ok {
		return nil, nil
	}
	s, ok := raw.(string)
	if !ok || !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
		return nil, errors.New("endpoint must be an http(s) URL")
	}
	return aws.String(s), nil
}

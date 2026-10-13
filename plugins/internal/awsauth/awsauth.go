// Package awsauth costruisce la configurazione dell'SDK AWS per i plugin ecs e lambda: region
// del target, credenziali dai segreti di manent o dalla catena standard dell'SDK (variabili
// d'ambiente, ruolo IRSA, ruolo del task ECS o dell'istanza EC2) ed eventuale assume role.
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

// Parametri del descrittore (deploy.params) comuni ai plugin AWS.
const (
	ParamAccessKeySecret    = "access_key_id_secret"     // segreto con l'access key ID
	ParamSecretKeySecret    = "secret_access_key_secret" // segreto con la secret access key
	ParamSessionTokenSecret = "session_token_secret"     // segreto con il session token (credenziali temporanee)
	ParamRoleARN            = "role_arn"                 // ruolo da assumere con STS
	ParamExternalID         = "external_id"              // external ID richiesto dal ruolo
)

var (
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	roleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[\w+=,.@/-]+$`)
	sessionRe = regexp.MustCompile(`[^\w+=,.@-]`)
)

// ValidRegion indica se s è il nome di una region AWS (es. eu-west-1).
func ValidRegion(s string) bool { return regionRe.MatchString(s) }

// Validate controlla i parametri delle credenziali.
func Validate(params map[string]string) error {
	var errs []error
	if (params[ParamAccessKeySecret] == "") != (params[ParamSecretKeySecret] == "") {
		errs = append(errs, fmt.Errorf("%q e %q vanno indicati insieme", ParamAccessKeySecret, ParamSecretKeySecret))
	}
	if params[ParamSessionTokenSecret] != "" && params[ParamAccessKeySecret] == "" {
		errs = append(errs, fmt.Errorf("%q richiede %q e %q", ParamSessionTokenSecret, ParamAccessKeySecret, ParamSecretKeySecret))
	}
	if r := params[ParamRoleARN]; r != "" && !roleARNRe.MatchString(r) {
		errs = append(errs, fmt.Errorf("%s %q non valido: atteso arn:aws:iam::<account>:role/<nome>", ParamRoleARN, r))
	}
	if params[ParamExternalID] != "" && params[ParamRoleARN] == "" {
		errs = append(errs, fmt.Errorf("%q richiede %q", ParamExternalID, ParamRoleARN))
	}
	return errors.Join(errs...)
}

// Config restituisce la configurazione dell'SDK per la region. Le credenziali statiche sono
// lette dai segreti solo qui, al momento dell'uso, e non vengono mai registrate.
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
		return aws.Config{}, fmt.Errorf("configurazione AWS: %w", err)
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

// SessionName è il nome della sessione STS: riporta l'ID del deploy, così CloudTrail collega
// le chiamate al deploy che le ha fatte.
func SessionName(deploymentID string) string {
	s := "manent-" + sessionRe.ReplaceAllString(deploymentID, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// Endpoint restituisce l'endpoint alternativo dalla configurazione del plugin (es. LocalStack),
// nil se non indicato.
func Endpoint(cfg map[string]any) (*string, error) {
	raw, ok := cfg["endpoint"]
	if !ok {
		return nil, nil
	}
	s, ok := raw.(string)
	if !ok || !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
		return nil, errors.New("endpoint deve essere un URL http(s)")
	}
	return aws.String(s), nil
}

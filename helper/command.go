package helper 

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/go-hclog"
	"github.com/docker/docker-credential-helpers/credentials"
	"github.com/hashicorp/vault/command/agent/config"
	"github.com/hashicorp/vault/command/agent/auth"
	"github.com/hashicorp/vault/command/agent/auth/aws"
	"github.com/hashicorp/vault/command/agent/auth/azure"
	"github.com/hashicorp/vault/command/agent/auth/alicloud"
	"github.com/hashicorp/vault/command/agent/auth/gcp"
	"github.com/hashicorp/vault/command/agent/auth/jwt"
	"github.com/hashicorp/vault/command/agent/auth/kubernetes"
	"github.com/hashicorp/vault/command/agent/auth/approle"
	"github.com/hashicorp/vault/command/agent/sink"
	"github.com/hashicorp/vault/command/agent/sink/file"
	"github.com/morningconsult/docker-credential-vault-login/cache"
	"github.com/morningconsult/docker-credential-vault-login/vault"
	"github.com/morningconsult/docker-credential-vault-login/logging"
)

const (
	envConfigFile = "DOCKER_CREDS_CONFIG_FILE"
	defaultConfigFile = "/etc/docker-credential-vault-login/config.hcl"
)

var (
	notImplementedError = fmt.Errorf("not implemented")
	defaultTimeout = 10 * time.Second
)

type HelperOptions struct {
	Logger hclog.Logger
	Client *api.Client
}

type Helper struct {
	logger hclog.Logger
	client *api.Client
}

func NewHelper(opts *HelperOptions) *Helper {
	if opts == nil {
		opts = &HelperOptions{}
	}

	return &Helper{
		logger: opts.Logger,
		client: opts.Client,
	}
}

func (h *Helper) Add(creds *credentials.Credentials) error {
	return notImplementedError
}

func (h *Helper) Delete(serverURL string) error {
	return notImplementedError
}

func (h *Helper) List() (map[string]string, error) {
	return nil, notImplementedError
}

func (h *Helper) Get(serverURL string) (string, string, error) {
	// Create new logger
	if h.logger == nil {
		opts := &hclog.LoggerOptions{
			Name:   "helper.get",
			Level:  hclog.Error,
			Output: os.Stderr,
		}

		w, err := logging.LogWriter(nil)
		if err != nil {
			h.logger.Error("Error opening log file. Logging errors to stderr instead.", "error", err)
		} else {
			opts.Output = w
			defer w.Close()
		}

		h.logger = hclog.New(opts)
	}

	configFile := defaultConfigFile
	if f := os.Getenv(envConfigFile); f != "" {
		configFile = f
	}

	config, err := config.LoadConfig(configFile, h.logger)
	if err != nil {
		h.logger.Error(fmt.Sprintf("Error loading configuration from %s", configFile), "error", err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	if config == nil {
		h.logger.Error("No configuration read. Please provide the configuration file with the " +
			envConfigFile + " environment variable.")
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	if config.AutoAuth == nil {
		h.logger.Error(fmt.Sprintf("No auto_auth block found in configuration file %s", configFile))
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	secretRaw, ok := config.AutoAuth.Method.Config["secret"]
	if !ok {
		h.logger.Error(fmt.Sprintf("No 'secret' field found in auto_auth.method.config of " +
			"configuration file %s", configFile))
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	secret, ok := secretRaw.(string)
	if !ok {
		h.logger.Error(fmt.Sprintf("field auto_auth.method.config.secret of configuration file %s " +
			"could not be converted to string", configFile))
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	if h.client == nil {
		h.client, err = api.NewClient(nil)
		if err != nil {
			h.logger.Error("Error creating new Vault API client", "error", err)
			return "", "", credentials.NewErrCredentialsNotFound()
		}
	}

	cloned, _ := h.client.Clone()

	// Get any cached tokens
	cachedTokens, err := cache.GetCachedTokens(config.AutoAuth.Sinks, cloned)
	if err != nil {
		h.logger.Error("Error getting cached token(s). Re-authenticating.", "error", err)
	}

	// Renew the cached tokens
	for _, token := range cachedTokens {
		if _, err := h.client.Auth().Token().RenewTokenAsSelf(token, 0); err != nil {
			h.logger.Error("Error renewing token", "error", err)
		}
	}

	// Use any token to get credentials
	for _, token := range cachedTokens {
		h.client.SetToken(token)

		// Get credentials
		creds, err := vault.GetCredentials(secret, h.client)
		if err != nil {
			h.logger.Error("Error reading secret from Vault", "error", err)
			continue
		}
		return creds.Username, creds.Password, nil
	}

	// Failed to read secret with cached token. Reauthenticate.
	h.client.ClearToken()

	sinks, err := h.buildSinks(config.AutoAuth.Sinks)
	if err != nil {
		h.logger.Error("Error building sinks", "error", err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	method, err := h.buildMethod(config.AutoAuth.Method)
	if err != nil {
		h.logger.Error("Error building method", "error", err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}

	ss := sink.NewSinkServer(&sink.SinkServerConfig{
		Logger:        h.logger.Named("sink.server"),
		Client:        h.client,
		ExitAfterAuth: true,
	})

	ah := auth.NewAuthHandler(&auth.AuthHandlerConfig{
		Logger:  h.logger.Named("auth.handler"),
		Client:  h.client,
		WrapTTL: config.AutoAuth.Method.WrapTTL,
	})

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)

	newTokenCh := make(chan string)

	go ah.Run(ctx, method)
	go ss.Run(ctx, newTokenCh, sinks)

	var token string
	select {
	case <-ctx.Done():
		h.logger.Error(fmt.Sprintf("Failed to get credentials within timeout (%s)", defaultTimeout.String()))
		<-ah.DoneCh
		<-ss.DoneCh
		return "", "", credentials.NewErrCredentialsNotFound()
	case token = <-ah.OutputCh:
		// will have to unwrap if wrapped
		h.logger.Info("Successfully authenticated")
	}

	newTokenCh <- token

	select {
	case <-ctx.Done():
		h.logger.Error(fmt.Sprintf("Failed to write token to sink(s) within the timeout (%s)", defaultTimeout.String()))
		<-ah.DoneCh
		<-ss.DoneCh
		return "", "", credentials.NewErrCredentialsNotFound()
	case <-ss.DoneCh:
		h.logger.Info("Successfully wrote token to sink(s)")
	}

	cancel()
	<-ah.DoneCh

	h.client.SetToken(token)

	// Get credentials
	creds, err := vault.GetCredentials(secret, h.client)
	if err != nil {
		h.logger.Error("Error reading secret from Vault", "error", err)
		return "", "", credentials.NewErrCredentialsNotFound()
	}
	return creds.Username, creds.Password, nil
}

func (h *Helper) buildSinks(ss []*config.Sink) ([]*sink.SinkConfig, error) {
	var sinks []*sink.SinkConfig
	for _, sc := range ss {
		switch sc.Type {
		case "file":
			config := &sink.SinkConfig{
				Logger:  h.logger.Named("sink.file"),
				Config:  sc.Config,
				Client:  h.client,
				WrapTTL: sc.WrapTTL,
				DHType:  sc.DHType,
				DHPath:  sc.DHPath,
				AAD:     sc.AAD,
			}
			s, err := file.NewFileSink(config)
			if err != nil {
				return nil, fmt.Errorf("Error creating file sink: %v", err)
			}
			config.Sink = s
			sinks = append(sinks, config)
		default:
			return nil, fmt.Errorf("Unknown sink type %q", sc.Type)
		}
	}
	return sinks, nil
}

func (h *Helper) buildMethod(config *config.Method) (auth.AuthMethod, error) {
	var (
		method auth.AuthMethod
		err error
	)

	authConfig := &auth.AuthConfig{
		Logger:    h.logger.Named(fmt.Sprintf("auth.%s", config.Type)),
		MountPath: config.MountPath,
		Config:    config.Config,
	}
	switch config.Type {
	case "alicloud":
		method, err = alicloud.NewAliCloudAuthMethod(authConfig)
	case "aws":
		method, err = aws.NewAWSAuthMethod(authConfig)
	case "azure":
		method, err = azure.NewAzureAuthMethod(authConfig)
	case "gcp":
		method, err = gcp.NewGCPAuthMethod(authConfig)
	case "jwt":
		method, err = jwt.NewJWTAuthMethod(authConfig)
	case "kubernetes":
		method, err = kubernetes.NewKubernetesAuthMethod(authConfig)
	case "approle":
		method, err = approle.NewApproleAuthMethod(authConfig)
	default:
		return nil, fmt.Errorf("Unknown auth method %q", config.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("Error creating %s auth method: %v", config.Type, err)
	}
	return method, nil
}


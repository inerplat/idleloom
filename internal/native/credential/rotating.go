package credential

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const maxTokenResponseBytes = 1 << 20

type Options struct {
	Namespace      string
	ServiceAccount string
	KubeconfigPath string
	RefreshBefore  time.Duration
	TokenDuration  time.Duration
	Now            func() time.Time
	Logf           func(string, ...any)
}

type source struct {
	mu               sync.Mutex
	token            string
	expiresAt        time.Time
	apiServer        string
	namespace        string
	serviceAccount   string
	kubeconfigPath   string
	refreshBefore    time.Duration
	tokenDuration    time.Duration
	now              func() time.Time
	logf             func(string, ...any)
	refreshTransport http.RoundTripper
}

type roundTripper struct {
	base   http.RoundTripper
	source *source
}

func Configure(config *rest.Config, options Options) (*rest.Config, error) {
	if config == nil || config.Host == "" || config.BearerToken == "" {
		return nil, fmt.Errorf("a bearer-token Kubernetes config is required")
	}
	if options.Namespace == "" || options.ServiceAccount == "" {
		return nil, fmt.Errorf("service account namespace and name are required")
	}
	expiresAt, err := tokenExpiry(config.BearerToken)
	if err != nil {
		return nil, err
	}
	refreshBefore := options.RefreshBefore
	if refreshBefore <= 0 {
		refreshBefore = 15 * time.Minute
	}
	tokenDuration := options.TokenDuration
	if tokenDuration <= 0 || tokenDuration > 24*time.Hour {
		tokenDuration = 8 * time.Hour
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	transport, err := rest.TransportFor(rest.AnonymousClientConfig(config))
	if err != nil {
		return nil, fmt.Errorf("create token refresh transport: %w", err)
	}
	source := &source{
		token: config.BearerToken, expiresAt: expiresAt, apiServer: strings.TrimRight(config.Host, "/"),
		namespace: options.Namespace, serviceAccount: options.ServiceAccount,
		kubeconfigPath: options.KubeconfigPath, refreshBefore: refreshBefore,
		tokenDuration: tokenDuration, now: now, logf: options.Logf, refreshTransport: transport,
	}
	copy := rest.CopyConfig(config)
	copy.BearerToken = ""
	copy.BearerTokenFile = ""
	previous := copy.WrapTransport
	copy.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		if previous != nil {
			base = previous(base)
		}
		return &roundTripper{base: base, source: source}
	}
	return copy, nil
}

func (r *roundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	token, err := r.source.currentToken(request.Context())
	if err != nil {
		return nil, err
	}
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+token)
	return r.base.RoundTrip(copy)
}

func (s *source) currentToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now().Before(s.expiresAt.Add(-s.refreshBefore)) {
		return s.token, nil
	}
	token, expiresAt, err := s.refresh(ctx)
	if err != nil {
		if !s.now().Before(s.expiresAt) {
			return "", fmt.Errorf("restricted Kubernetes credential expired and could not be refreshed; rerun join: %w", err)
		}
		return "", fmt.Errorf("refresh restricted Kubernetes credential: %w", err)
	}
	if s.kubeconfigPath != "" {
		if err := persistToken(s.kubeconfigPath, token); err != nil {
			return "", fmt.Errorf("persist refreshed Kubernetes token: %w", err)
		}
	}
	s.token = token
	s.expiresAt = expiresAt
	if s.logf != nil {
		s.logf("refreshed restricted Kubernetes credential; expires %s", expiresAt.Format(time.RFC3339))
	}
	return s.token, nil
}

func (s *source) refresh(ctx context.Context) (string, time.Time, error) {
	seconds := int64(s.tokenDuration / time.Second)
	body, err := json.Marshal(authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}})
	if err != nil {
		return "", time.Time{}, err
	}
	endpoint := s.apiServer + "/api/v1/namespaces/" + url.PathEscape(s.namespace) + "/serviceaccounts/" + url.PathEscape(s.serviceAccount) + "/token"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.refreshTransport.RoundTrip(request)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("refresh Kubernetes token: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes+1))
	if err != nil {
		return "", time.Time{}, err
	}
	if len(data) > maxTokenResponseBytes {
		return "", time.Time{}, fmt.Errorf("token refresh response exceeded 1 MiB")
	}
	if response.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("token refresh returned %s", response.Status)
	}
	var result authenticationv1.TokenRequest
	if err := json.Unmarshal(data, &result); err != nil {
		return "", time.Time{}, err
	}
	if result.Status.Token == "" || result.Status.ExpirationTimestamp.IsZero() {
		return "", time.Time{}, fmt.Errorf("token refresh returned an incomplete credential")
	}
	return result.Status.Token, result.Status.ExpirationTimestamp.Time, nil
}

func tokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("restricted Kubernetes token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decode Kubernetes token expiry: %w", err)
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 {
		return time.Time{}, fmt.Errorf("restricted Kubernetes token has no valid expiry")
	}
	return time.Unix(claims.ExpiresAt, 0), nil
}

func persistToken(path, token string) error {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return err
	}
	context := config.Contexts[config.CurrentContext]
	if context == nil || config.AuthInfos[context.AuthInfo] == nil {
		return fmt.Errorf("kubeconfig current context has no auth info")
	}
	config.AuthInfos[context.AuthInfo].Token = token
	data, err := clientcmd.Write(*config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

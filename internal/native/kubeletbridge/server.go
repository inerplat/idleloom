package kubeletbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Target struct {
	AssignmentUID string
	Namespace     string
	PodName       string
	ContainerName string
}

type ServerConfig struct {
	ListenAddress              string
	Identity                   Identity
	ClientCA                   []byte
	AllowedClientCommonNames   []string
	AllowedClientOrganizations []string
	Logs                       *LogBuffer
	ResolveTarget              func() (Target, bool)
	ReadHeaderLimit            time.Duration
}

type Server struct {
	config ServerConfig
	server *http.Server
}

func NewServer(config ServerConfig) (*Server, error) {
	if config.ListenAddress == "" {
		config.ListenAddress = "0.0.0.0:10250"
	}
	if config.Identity.CertificateFile == "" || config.Identity.PrivateKeyFile == "" || len(config.ClientCA) == 0 || config.Logs == nil || config.ResolveTarget == nil {
		return nil, fmt.Errorf("kubelet bridge identity, client CA, log buffer, and target resolver are required")
	}
	if len(config.AllowedClientCommonNames) == 0 && len(config.AllowedClientOrganizations) == 0 {
		config.AllowedClientCommonNames = []string{"kube-apiserver-kubelet-client"}
		config.AllowedClientOrganizations = []string{"system:masters"}
	}
	return &Server{config: config}, nil
}

func (server *Server) Run(ctx context.Context) error {
	certificate, err := tls.LoadX509KeyPair(server.config.Identity.CertificateFile, server.config.Identity.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("load kubelet bridge serving identity: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(server.config.ClientCA) {
		return fmt.Errorf("parse Kubernetes client CA for kubelet bridge")
	}
	listener, err := net.Listen("tcp", server.config.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for kubelet bridge: %w", err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{certificate}, ClientCAs: clientCAs,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.authorize(server.handleHealth))
	mux.HandleFunc("/containerLogs/", server.authorize(server.handleLogs))
	readHeaderTimeout := server.config.ReadHeaderLimit
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = 5 * time.Second
	}
	server.server = &http.Server{
		Handler: mux, ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 30 * time.Second,
	}
	errors := make(chan error, 1)
	go func() {
		serveErr := server.server.Serve(tlsListener)
		if serveErr != nil && serveErr != http.ErrServerClosed {
			errors <- serveErr
		}
		close(errors)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.server.Shutdown(shutdownCtx)
	case err := <-errors:
		return err
	}
}

func (server *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || !isKubeletClient(request.TLS.PeerCertificates[0], server.config.AllowedClientCommonNames, server.config.AllowedClientOrganizations) {
			http.Error(response, "forbidden", http.StatusForbidden)
			return
		}
		next(response, request)
	}
}

func isKubeletClient(certificate *x509.Certificate, commonNames, organizations []string) bool {
	if len(commonNames) == 0 && len(organizations) == 0 {
		commonNames = []string{"kube-apiserver-kubelet-client"}
		organizations = []string{"system:masters"}
	}
	if containsString(commonNames, certificate.Subject.CommonName) {
		return true
	}
	for _, organization := range certificate.Subject.Organization {
		if containsString(organizations, organization) {
			return true
		}
	}
	return false
}

func (server *Server) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = response.Write([]byte("ok"))
}

func (server *Server) handleLogs(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target, ok := server.config.ResolveTarget()
	if !ok {
		http.Error(response, "no native assignment is running", http.StatusNotFound)
		return
	}
	namespace, pod, container, ok := parseContainerLogsPath(request.URL.Path)
	if !ok || namespace != target.Namespace || pod != target.PodName || container != target.ContainerName {
		http.Error(response, "projected container not found", http.StatusNotFound)
		return
	}
	options, err := parseLogOptions(request.URL.Query(), time.Now().UTC())
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !options.follow {
		for _, entry := range server.config.Logs.Snapshot(target.AssignmentUID, options.since, options.tailLines) {
			writeLogEntry(response, entry, options.timestamps)
		}
		return
	}
	initial, cursor, notifications, unsubscribe := server.config.Logs.SnapshotAndSubscribe(target.AssignmentUID, options.since, options.tailLines)
	defer unsubscribe()
	for _, entry := range initial {
		writeLogEntry(response, entry, options.timestamps)
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		http.Error(response, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	flusher.Flush()
	for {
		select {
		case <-request.Context().Done():
			return
		case _, open := <-notifications:
			if !open {
				return
			}
			entries, gap := server.config.Logs.EntriesAfter(target.AssignmentUID, cursor)
			if gap {
				_, _ = fmt.Fprintln(response, "idleloom: log stream fell behind; older entries were evicted")
			}
			for _, entry := range entries {
				writeLogEntry(response, entry, options.timestamps)
				cursor = entry.Sequence
			}
			flusher.Flush()
		}
	}
}

type logOptions struct {
	follow     bool
	timestamps bool
	tailLines  int64
	since      time.Time
}

func parseLogOptions(values url.Values, now time.Time) (logOptions, error) {
	options := logOptions{tailLines: -1}
	var err error
	if options.follow, err = parseBool(values.Get("follow")); err != nil {
		return logOptions{}, err
	}
	if options.timestamps, err = parseBool(values.Get("timestamps")); err != nil {
		return logOptions{}, err
	}
	if value := values.Get("tailLines"); value != "" {
		options.tailLines, err = strconv.ParseInt(value, 10, 64)
		if err != nil || options.tailLines < 0 {
			return logOptions{}, fmt.Errorf("invalid tailLines")
		}
	}
	if value := values.Get("sinceSeconds"); value != "" {
		seconds, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || seconds < 0 {
			return logOptions{}, fmt.Errorf("invalid sinceSeconds")
		}
		options.since = now.Add(-time.Duration(seconds) * time.Second)
	}
	return options, nil
}

func parseBool(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid boolean option")
	}
	return parsed, nil
}

func parseContainerLogsPath(path string) (string, string, string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/containerLogs/"), "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	for index := range parts {
		value, err := url.PathUnescape(parts[index])
		if err != nil || value == "" || strings.Contains(value, "/") {
			return "", "", "", false
		}
		parts[index] = value
	}
	return parts[0], parts[1], parts[2], true
}

func writeLogEntry(response http.ResponseWriter, entry LogEntry, timestamps bool) {
	if timestamps {
		_, _ = fmt.Fprintf(response, "%s %s\n", entry.Time.Format(time.RFC3339Nano), entry.Message)
		return
	}
	_, _ = fmt.Fprintln(response, entry.Message)
}

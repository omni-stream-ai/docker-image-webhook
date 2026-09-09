package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const maxBodyBytes = 64 << 10

var (
	repositoryPartPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	portPattern           = regexp.MustCompile(`^[0-9]{1,5}$`)
	tagPattern            = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

type payload struct {
	Image    string `json:"image"`
	Tag      string `json:"tag"`
	Registry string `json:"registry"`
	PushData struct {
		Digest   string `json:"digest"`
		PushedAt string `json:"pushed_at"`
		Tag      string `json:"tag"`
	} `json:"push_data"`
	Repository repositoryPayload `json:"repository"`
}

// repositoryPayload accepts both the object shape used by Alibaba Cloud and
// the string shape used by several generic registry webhooks.
type repositoryPayload struct {
	Image        string `json:"image"`
	Registry     string `json:"registry"`
	FullName     string `json:"full_name"`
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Region       string `json:"region"`
	RepoFullName string `json:"repo_full_name"`
}

func (p *repositoryPayload) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &p.Image)
	}
	type alias repositoryPayload
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*p = repositoryPayload(value)
	return nil
}

type config struct {
	listenAddr       string
	secret           string
	engine           string
	fixedRepository  string
	allowedRepos     map[string]struct{}
	pullTimeout      time.Duration
	registryTemplate string
	postPullCommand  string
}

type server struct {
	config   config
	pulling  atomic.Bool
	pull     func(context.Context, string) error
	postPull func(context.Context, string) error
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	s := &server{config: cfg}
	s.pull = s.pullImage
	s.postPull = s.runPostPull
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/payload", s.payloadHandler)

	httpServer := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("image pull webhook listening on %s using %s", cfg.listenAddr, cfg.engine)
	log.Fatal(httpServer.ListenAndServe())
}

func loadConfig() (config, error) {
	cfg := config{
		listenAddr:       envOr("WEBHOOK_LISTEN_ADDR", "127.0.0.1:19090"),
		secret:           os.Getenv("WEBHOOK_SECRET"),
		engine:           envOr("WEBHOOK_CONTAINER_ENGINE", "podman"),
		fixedRepository:  strings.TrimSpace(os.Getenv("WEBHOOK_IMAGE_REPOSITORY")),
		allowedRepos:     parseList(os.Getenv("WEBHOOK_ALLOWED_REPOSITORIES")),
		pullTimeout:      10 * time.Minute,
		registryTemplate: envOr("WEBHOOK_REGISTRY_TEMPLATE", "registry.%s.aliyuncs.com"),
		postPullCommand:  strings.TrimSpace(os.Getenv("WEBHOOK_POST_PULL_COMMAND")),
	}
	if cfg.secret == "" {
		return config{}, errors.New("WEBHOOK_SECRET is required")
	}
	if cfg.engine != "podman" && cfg.engine != "docker" {
		return config{}, errors.New("WEBHOOK_CONTAINER_ENGINE must be podman or docker")
	}
	if cfg.fixedRepository == "" && len(cfg.allowedRepos) == 0 {
		return config{}, errors.New("set WEBHOOK_IMAGE_REPOSITORY or WEBHOOK_ALLOWED_REPOSITORIES")
	}
	if value := os.Getenv("WEBHOOK_PULL_TIMEOUT"); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return config{}, errors.New("WEBHOOK_PULL_TIMEOUT must be a positive duration")
		}
		cfg.pullTimeout = d
	}
	if strings.Count(cfg.registryTemplate, "%s") != 1 {
		return config{}, errors.New("WEBHOOK_REGISTRY_TEMPLATE must contain exactly one %s placeholder")
	}
	return cfg, nil
}

func (s *server) payloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !validSecret(s.config.secret, r.URL.Query().Get("secret")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	var event payload
	if err := decoder.Decode(&event); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if err := ensureEOF(decoder); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}

	image, err := s.imageFor(event)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.pulling.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "an image update is already running"})
		return
	}
	go s.executeImageUpdate(image, event.PushData.Digest)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "image": image})
}

func (s *server) executeImageUpdate(image, digest string) {
	defer s.pulling.Store(false)

	pullCtx, cancelPull := context.WithTimeout(context.Background(), s.config.pullTimeout)
	defer cancelPull()
	log.Printf("pulling image %s (digest %s)", image, digest)
	if err := s.pull(pullCtx, image); err != nil {
		log.Printf("image pull failed for %s: %v", image, err)
		return
	}
	cancelPull()
	log.Printf("image pull completed for %s", image)
	if s.config.postPullCommand == "" {
		return
	}

	postPullCtx, cancelPostPull := context.WithTimeout(context.Background(), s.config.pullTimeout)
	defer cancelPostPull()
	postPull := s.postPull
	if postPull == nil {
		postPull = s.runPostPull
	}
	if err := postPull(postPullCtx, image); err != nil {
		log.Printf("post-pull command failed for %s: %v", image, err)
		return
	}
	log.Printf("post-pull command completed for %s", image)
}

func (s *server) imageFor(event payload) (string, error) {
	tag := strings.TrimSpace(event.Tag)
	if tag == "" {
		tag = strings.TrimSpace(event.PushData.Tag)
	}
	if !tagPattern.MatchString(tag) {
		return "", errors.New("invalid repository or tag")
	}

	if s.config.fixedRepository != "" {
		if !validConfiguredRepository(s.config.fixedRepository) {
			return "", errors.New("invalid configured image repository")
		}
		return s.config.fixedRepository + ":" + tag, nil
	}

	image := strings.TrimSpace(event.Image)
	if image == "" {
		image = strings.TrimSpace(event.Repository.Image)
	}
	fullName := strings.TrimSpace(event.Repository.RepoFullName)
	if fullName == "" {
		fullName = strings.TrimSpace(event.Repository.FullName)
	}
	if fullName == "" {
		fullName = strings.TrimSpace(event.Repository.Namespace + "/" + event.Repository.Name)
	}
	registry := strings.TrimSpace(event.Registry)
	if registry == "" {
		registry = strings.TrimSpace(event.Repository.Registry)
	}
	if image == "" && registry != "" && validRegistryHost(registry) && validRepository(fullName) {
		image = registry + "/" + fullName
	}
	if image == "" && event.Repository.Region != "" && validRepository(fullName) {
		if !repositoryPartPattern.MatchString(event.Repository.Region) {
			return "", errors.New("invalid region")
		}
		image = fmt.Sprintf(s.config.registryTemplate, event.Repository.Region) + "/" + fullName
	}
	if !validImageReference(image) || !repositoryAllowed(image, fullName, s.config.allowedRepos) {
		return "", errors.New("repository is not allowed or image is invalid")
	}
	return image + ":" + tag, nil
}

func repositoryAllowed(image, fullName string, allowed map[string]struct{}) bool {
	repository := image
	if index := strings.LastIndex(repository, ":"); index > strings.LastIndex(repository, "/") {
		repository = repository[:index]
	}
	if _, ok := allowed[repository]; ok {
		return true
	}
	_, ok := allowed[fullName]
	return ok
}

func (s *server) pullImage(ctx context.Context, image string) error {
	command := exec.CommandContext(ctx, s.config.engine, "pull", image)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		return fmt.Errorf("%w: %s", err, message)
	}
	return nil
}

func (s *server) runPostPull(ctx context.Context, image string) error {
	command := exec.CommandContext(ctx, "/bin/sh", "-c", s.config.postPullCommand)
	command.Env = append(os.Environ(), "WEBHOOK_IMAGE="+image)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func validSecret(expected, actual string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	actualHash := sha256.Sum256([]byte(actual))
	return subtle.ConstantTimeCompare(expectedHash[:], actualHash[:]) == 1
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		return false
	}
	return validRepositoryPath(parts)
}

func validRepositoryPath(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !repositoryPartPattern.MatchString(part) {
			return false
		}
	}
	return true
}

func validImageRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || !validRegistryHost(parts[0]) {
		return false
	}
	return validRepositoryPath(parts[1:])
}

func validConfiguredRepository(value string) bool {
	if validImageRepository(value) || validRepository(value) {
		return true
	}
	return repositoryPartPattern.MatchString(value)
}

func validImageReference(value string) bool {
	if strings.ContainsAny(value, "@ \t\r\n") || value == "" {
		return false
	}
	if index := strings.LastIndex(value, ":"); index > strings.LastIndex(value, "/") {
		value = value[:index]
	}
	parts := strings.Split(value, "/")
	if len(parts) == 1 {
		return repositoryPartPattern.MatchString(value)
	}
	if validRegistryHost(parts[0]) && validRepositoryPath(parts[1:]) {
		return true
	}
	return validRepositoryPath(parts)
}

func validRegistryHost(value string) bool {
	if host, port, found := strings.Cut(value, ":"); found {
		if !repositoryPartPattern.MatchString(host) || !portPattern.MatchString(port) {
			return false
		}
		value = host
	}
	if len(value) > 253 || strings.Contains(value, "..") {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if !repositoryPartPattern.MatchString(part) {
			return false
		}
	}
	return true
}

func parseList(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("unexpected data after payload")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

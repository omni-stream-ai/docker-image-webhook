package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPayloadPullsAllowedImage(t *testing.T) {
	cfg := testConfig()
	s := &server{config: cfg}
	var pulled string
	s.pull = func(_ context.Context, image string) error {
		pulled = image
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/payload?secret=test-secret", strings.NewReader(testPayload("namespace/repo-test", "latest")))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.payloadHandler(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if pulled != "registry.cn-hangzhou.aliyuncs.com/namespace/repo-test:latest" {
		t.Fatalf("pulled image = %q", pulled)
	}
}

func TestPayloadAcceptsAdditionalAliyunFields(t *testing.T) {
	s := &server{config: testConfig()}
	s.pull = func(_ context.Context, _ string) error { return nil }
	body := strings.Replace(
		testPayload("namespace/repo-test", "latest"),
		`"repo_full_name":"namespace/repo-test"`,
		`"repo_full_name":"namespace/repo-test","repo_authentication_type":"NO_CERTIFIED","repo_origin_type":"NO_CERTIFIED","repo_type":"PUBLIC","date_created":"2016-10-28 21:31:42"`,
		1,
	)
	req := httptest.NewRequest(http.MethodPost, "/payload?secret=test-secret", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.payloadHandler(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestPayloadPullsGenericRegistryImages(t *testing.T) {
	for name, body := range map[string]string{
		"top-level image":   `{"image":"ghcr.io/acme/widget","tag":"v1.2.3"}`,
		"string repository": `{"repository":"registry.example.com:5000/team/service","tag":"stable"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.allowedRepos = map[string]struct{}{
				"ghcr.io/acme/widget":                    {},
				"registry.example.com:5000/team/service": {},
			}
			var pulled string
			s := &server{config: cfg, pull: func(_ context.Context, image string) error {
				pulled = image
				return nil
			}}
			req := httptest.NewRequest(http.MethodPost, "/payload?secret=test-secret", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			s.payloadHandler(response, req)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if pulled == "" {
				t.Fatal("pull was not called")
			}
		})
	}
}

func TestFixedRepositorySupportsDockerHubStyleName(t *testing.T) {
	cfg := testConfig()
	cfg.fixedRepository = "acme/widget"
	cfg.allowedRepos = nil
	s := &server{config: cfg}
	image, err := s.imageFor(payload{Tag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if image != "acme/widget:latest" {
		t.Fatalf("image = %q", image)
	}
}

func TestPayloadRunsPostPullCommandAfterPull(t *testing.T) {
	cfg := testConfig()
	cfg.postPullCommand = "/usr/local/bin/redeploy"
	var steps []string
	s := &server{
		config: cfg,
		pull: func(_ context.Context, image string) error {
			steps = append(steps, "pull:"+image)
			return nil
		},
		postPull: func(_ context.Context, image string) error {
			steps = append(steps, "post:"+image)
			return nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/payload?secret=test-secret", strings.NewReader(testPayload("namespace/repo-test", "latest")))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.payloadHandler(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	want := []string{"pull:registry.cn-hangzhou.aliyuncs.com/namespace/repo-test:latest", "post:registry.cn-hangzhou.aliyuncs.com/namespace/repo-test:latest"}
	if strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
}

func TestPayloadReportsPostPullFailure(t *testing.T) {
	cfg := testConfig()
	cfg.postPullCommand = "/usr/local/bin/redeploy"
	s := &server{
		config:   cfg,
		pull:     func(_ context.Context, _ string) error { return nil },
		postPull: func(_ context.Context, _ string) error { return errors.New("deploy failed") },
	}
	req := httptest.NewRequest(http.MethodPost, "/payload?secret=test-secret", strings.NewReader(testPayload("namespace/repo-test", "latest")))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.payloadHandler(response, req)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestPayloadRejectsUnauthorizedRequest(t *testing.T) {
	s := &server{config: testConfig(), pull: func(context.Context, string) error {
		t.Fatal("pull must not be called")
		return nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/payload?secret=wrong", strings.NewReader(testPayload("namespace/repo-test", "latest")))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.payloadHandler(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestPayloadRejectsUnlistedRepositoryAndInvalidTag(t *testing.T) {
	for name, body := range map[string]string{
		"unlisted repository": testPayload("other/repo", "latest"),
		"invalid tag":         testPayload("namespace/repo-test", "latest;id"),
	} {
		t.Run(name, func(t *testing.T) {
			s := &server{config: testConfig(), pull: func(context.Context, string) error {
				t.Fatal("pull must not be called")
				return nil
			}}
			req := httptest.NewRequest(http.MethodPost, "/payload?secret=test-secret", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			s.payloadHandler(response, req)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func testConfig() config {
	return config{
		secret:           "test-secret",
		engine:           "podman",
		allowedRepos:     map[string]struct{}{"namespace/repo-test": {}},
		pullTimeout:      time.Minute,
		registryTemplate: "registry.%s.aliyuncs.com",
	}
}

func testPayload(repository, tag string) string {
	parts := strings.Split(repository, "/")
	return `{"push_data":{"digest":"sha256:abc","pushed_at":"2026-01-01 00:00:00","tag":"` + tag + `"},` +
		`"repository":{"name":"` + parts[len(parts)-1] + `","namespace":"` + parts[0] + `","region":"cn-hangzhou","repo_full_name":"` + repository + `"}}`
}

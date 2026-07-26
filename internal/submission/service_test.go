package submission_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/submission"
	submissionfilesystem "github.com/araihu/paje/internal/submission/filesystem"
	submissionmock "github.com/araihu/paje/internal/submission/mock"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
)

func validStoreReservation() submission.Reservation {
	createdAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	return submission.Reservation{
		IdempotencyKey: strings.Repeat("a", 32),
		Record: submission.Record{
			RunID:                "paje_abc",
			CredentialID:         testCredentialID,
			RequestDigest:        strings.Repeat("1", 64),
			IdempotencyKeyDigest: strings.Repeat("2", 64),
			Template:             templatecodechange.ID,
			CanonicalInput:       json.RawMessage(`{"task_description":"change timeout"}`),
			Origin: submission.Origin{
				Harness: "codex", SessionID: "session-1", TurnID: "turn-1",
			},
			RootRunID: "paje_abc",
			Depth:     0,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	}
}

func TestStoreContract(t *testing.T) {
	tests := []struct {
		name string
		new  func(*testing.T) submission.Store
	}{
		{
			name: "mock",
			new: func(*testing.T) submission.Store {
				return submissionmock.NewStore()
			},
		},
		{
			name: "filesystem",
			new: func(t *testing.T) submission.Store {
				t.Helper()
				store, err := submissionfilesystem.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Run("exact reuse", func(t *testing.T) {
				store := test.new(t)
				reservation := validStoreReservation()
				first, owned, err := store.Reserve(context.Background(), reservation)
				if err != nil || !owned {
					t.Fatalf("first Reserve() = (%#v, %v, %v), want owner", first, owned, err)
				}
				second, owned, err := store.Reserve(context.Background(), reservation)
				if err != nil || owned || second.RunID != first.RunID {
					t.Fatalf("second Reserve() = (%#v, %v, %v), want exact reuse", second, owned, err)
				}
			})

			t.Run("changed digest conflicts", func(t *testing.T) {
				store := test.new(t)
				reservation := validStoreReservation()
				if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
					t.Fatal(err)
				}
				reservation.Record.RequestDigest = strings.Repeat("f", 64)
				_, _, err := store.Reserve(context.Background(), reservation)
				if !errors.Is(err, submission.ErrIdempotencyConflict) {
					t.Fatalf("changed Reserve() error = %v, want idempotency conflict", err)
				}
			})

			t.Run("thirty two exact reservations have one owner", func(t *testing.T) {
				store := test.new(t)
				reservation := validStoreReservation()
				type result struct {
					record submission.Record
					owned  bool
					err    error
				}
				results := make(chan result, 32)
				var wait sync.WaitGroup
				for range 32 {
					wait.Add(1)
					go func() {
						defer wait.Done()
						record, owned, err := store.Reserve(context.Background(), reservation)
						results <- result{record: record, owned: owned, err: err}
					}()
				}
				wait.Wait()
				close(results)
				owners := 0
				for result := range results {
					if result.err != nil {
						t.Fatal(result.err)
					}
					if result.record.RunID != reservation.Record.RunID {
						t.Fatalf("Reserve() run ID = %q, want %q", result.record.RunID, reservation.Record.RunID)
					}
					if result.owned {
						owners++
					}
				}
				if owners != 1 {
					t.Fatalf("reservation owners = %d, want 1", owners)
				}
			})

			t.Run("different requests racing on one key conflict", func(t *testing.T) {
				store := test.new(t)
				left := validStoreReservation()
				right := validStoreReservation()
				right.Record.RequestDigest = strings.Repeat("f", 64)
				type result struct {
					owned bool
					err   error
				}
				results := make(chan result, 32)
				var wait sync.WaitGroup
				for index := range 32 {
					reservation := left
					if index%2 == 1 {
						reservation = right
					}
					wait.Add(1)
					go func() {
						defer wait.Done()
						_, owned, err := store.Reserve(context.Background(), reservation)
						results <- result{owned: owned, err: err}
					}()
				}
				wait.Wait()
				close(results)
				owners := 0
				conflicts := 0
				for result := range results {
					switch {
					case result.err == nil && result.owned:
						owners++
					case result.err == nil:
					case errors.Is(result.err, submission.ErrIdempotencyConflict):
						conflicts++
					default:
						t.Fatalf("Reserve() error = %v", result.err)
					}
				}
				if owners != 1 || conflicts != 16 {
					t.Fatalf("race owners/conflicts = %d/%d, want 1/16", owners, conflicts)
				}
			})

			t.Run("trigger binding is exact compare and swap", func(t *testing.T) {
				store := test.new(t)
				reservation := validStoreReservation()
				if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
					t.Fatal(err)
				}
				reference := submission.TriggerReference{Provider: "hatchet", ExternalRunID: "run-1"}
				first, err := store.BindTrigger(context.Background(), reservation.Record.RunID, reference)
				if err != nil || first.Trigger == nil || *first.Trigger != reference {
					t.Fatalf("first BindTrigger() = (%#v, %v)", first, err)
				}
				second, err := store.BindTrigger(context.Background(), reservation.Record.RunID, reference)
				if err != nil || second.Trigger == nil || *second.Trigger != reference {
					t.Fatalf("second BindTrigger() = (%#v, %v)", second, err)
				}
				changed := submission.TriggerReference{Provider: "hatchet", ExternalRunID: "run-2"}
				_, err = store.BindTrigger(context.Background(), reservation.Record.RunID, changed)
				if !errors.Is(err, submission.ErrIdempotencyConflict) {
					t.Fatalf("changed BindTrigger() error = %v, want idempotency conflict", err)
				}
			})

			t.Run("cancellation timestamp never moves", func(t *testing.T) {
				store := test.new(t)
				reservation := validStoreReservation()
				if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
					t.Fatal(err)
				}
				requestedAt := reservation.Record.UpdatedAt.Add(time.Minute)
				first, newlyRequested, err := store.MarkCancellationRequested(context.Background(), reservation.Record.RunID, requestedAt)
				if err != nil || !newlyRequested || first.CancellationRequested == nil || !first.CancellationRequested.Equal(requestedAt) {
					t.Fatalf("first MarkCancellationRequested() = (%#v, %t, %v)", first, newlyRequested, err)
				}
				for _, repeatedAt := range []time.Time{requestedAt.Add(-time.Second), requestedAt.Add(time.Hour)} {
					repeated, newlyRequested, err := store.MarkCancellationRequested(context.Background(), reservation.Record.RunID, repeatedAt)
					if err != nil || newlyRequested || repeated.CancellationRequested == nil || !repeated.CancellationRequested.Equal(requestedAt) || !repeated.UpdatedAt.Equal(requestedAt) {
						t.Fatalf("repeated MarkCancellationRequested(%s) = (%#v, %t, %v)", repeatedAt, repeated, newlyRequested, err)
					}
				}
			})

			t.Run("concurrent cancellation has one durable owner", func(t *testing.T) {
				store := test.new(t)
				reservation := validStoreReservation()
				if _, _, err := store.Reserve(context.Background(), reservation); err != nil {
					t.Fatal(err)
				}
				requestedAt := reservation.Record.UpdatedAt.Add(time.Minute)
				type result struct {
					record submission.Record
					owned  bool
					err    error
				}
				results := make(chan result, 32)
				var wait sync.WaitGroup
				for range 32 {
					wait.Add(1)
					go func() {
						defer wait.Done()
						record, owned, err := store.MarkCancellationRequested(
							context.Background(), reservation.Record.RunID, requestedAt,
						)
						results <- result{record: record, owned: owned, err: err}
					}()
				}
				wait.Wait()
				close(results)
				owners := 0
				for result := range results {
					if result.err != nil {
						t.Fatal(result.err)
					}
					if result.record.CancellationRequested == nil ||
						!result.record.CancellationRequested.Equal(requestedAt) {
						t.Fatalf("cancellation record = %#v", result.record)
					}
					if result.owned {
						owners++
					}
				}
				if owners != 1 {
					t.Fatalf("cancellation owners = %d, want 1", owners)
				}
			})
		})
	}
}

const (
	testCredentialID = "cred-codex-service"
	testUserID       = "codex@example.com"
	testAppID        = "service"
)

func testPrincipal() submission.Principal {
	return submission.Principal{
		CredentialID: testCredentialID,
		Subject:      testUserID,
		UserID:       testUserID,
		AppID:        testAppID,
		Repositories: []submission.RepositoryScope{{
			Host: "github.com", Owner: "example", Name: "service",
		}},
		Actions: map[submission.Action]bool{
			submission.ActionSubmitArtifact: true,
			submission.ActionRead:           true,
			submission.ActionCancel:         true,
		},
		Harnesses: map[string]bool{"codex": true},
		MaxDepth:  0,
	}
}

type serviceFixture struct {
	service *submission.Service
	store   *submissionmock.Store
	trigger *submissionmock.Trigger
}

func newTestService(t *testing.T) serviceFixture {
	t.Helper()
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	store := submissionmock.NewStore()
	trigger := submissionmock.NewTrigger()
	service, err := submission.New(submission.Dependencies{
		Templates:      registry,
		Store:          store,
		Trigger:        trigger,
		Clock:          func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
		SystemMaxDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return serviceFixture{service: service, store: store, trigger: trigger}
}

func testInput(repositoryURI, userID, appID, mode string) json.RawMessage {
	input := templatecodechange.Input{
		TaskDescription: "change timeout",
		RepositoryURI:   repositoryURI,
		BaseRef:         "main",
		Tags: map[string]string{
			"user_id": userID,
			"app_id":  appID,
		},
		WorkerProfile: "codex-go@1",
		Profile:       "generic",
		Checks: []verification.CommandSpec{{
			Name:       "test",
			Directory:  ".",
			Executable: "npm",
			Args:       []string{"test"},
			Timeout:    "10m",
			Required:   true,
		}},
		Publication: templatecodechange.Publication{Mode: mode},
	}
	if mode == "pull_request" {
		input.Publication.Provider = "github"
		input.Publication.TargetBranch = "main"
		input.Publication.Title = "Change timeout"
		input.Publication.Draft = true
	}
	raw, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return raw
}

func testSubmitRequest(raw json.RawMessage) submission.SubmitRequest {
	return submission.SubmitRequest{
		IdempotencyKey: strings.Repeat("a", 32),
		Template:       templatecodechange.ID,
		Input:          raw,
		Origin: submission.Origin{
			Harness:   "codex",
			SessionID: "session-1",
			TurnID:    "turn-1",
		},
	}
}

func TestSubmitBindsPrincipalAndRejectsClientIdempotencyField(t *testing.T) {
	t.Run("rejects caller-owned nested idempotency", func(t *testing.T) {
		fixture := newTestService(t)
		raw := json.RawMessage(`{
		  "idempotency_key":"client-must-not-set-this",
		  "task_description":"change timeout",
		  "repository_uri":"https://github.com/example/service.git",
		  "base_ref":"main",
		  "tags":{"user_id":"codex@example.com","app_id":"service"},
		  "profile":"generic",
		  "checks":[{
		    "name":"test","directory":".","executable":"npm",
		    "args":["test"],"timeout":"10m","required":true
		  }],
		  "publication":{"mode":"artifact"}
		}`)

		_, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(raw),
		)
		if !errors.Is(err, submission.ErrInvalidRequest) {
			t.Fatalf("Submit() error = %v, want invalid request", err)
		}
		if strings.Contains(err.Error(), "client-must-not-set-this") {
			t.Fatalf("Submit() error leaked input: %v", err)
		}
	})

	t.Run("binds exact identity and canonical repository", func(t *testing.T) {
		fixture := newTestService(t)
		view, reused, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(testInput(
				"https://GITHUB.com/example/service",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if reused {
			t.Fatal("Submit() reused = true, want first reservation")
		}
		if view.Record.CredentialID != testCredentialID {
			t.Fatalf("credential ID = %q, want %q", view.Record.CredentialID, testCredentialID)
		}
		var bound templatecodechange.Input
		if err := json.Unmarshal(view.Record.CanonicalInput, &bound); err != nil {
			t.Fatal(err)
		}
		if bound.Tags["user_id"] != testUserID || bound.Tags["app_id"] != testAppID {
			t.Fatalf("bound tags = %#v", bound.Tags)
		}
		if bound.RepositoryURI != "https://github.com/example/service.git" {
			t.Fatalf("repository = %q, want canonical GitHub URI", bound.RepositoryURI)
		}
		if bound.IdempotencyKey == "" || bound.IdempotencyKey == testSubmitRequest(nil).IdempotencyKey {
			t.Fatalf("nested idempotency key = %q, want server-derived key", bound.IdempotencyKey)
		}
		if view.Record.RootRunID != view.Record.RunID || view.Record.Depth != 0 {
			t.Fatalf("root/depth = %q/%d, want self/0", view.Record.RootRunID, view.Record.Depth)
		}
		if view.Status != submission.StatusAccepted {
			t.Fatalf("status = %q, want accepted", view.Status)
		}
	})
}

func TestSubmitRejectsChangedIdentityAndRepositoryScope(t *testing.T) {
	tests := []struct {
		name      string
		principal func(submission.Principal) submission.Principal
		raw       json.RawMessage
		want      error
	}{
		{
			name:      "user identity",
			principal: identityPrincipal,
			raw: testInput(
				"https://github.com/example/service.git",
				"other@example.com",
				testAppID,
				"artifact",
			),
			want: submission.ErrForbidden,
		},
		{
			name:      "application identity",
			principal: identityPrincipal,
			raw: testInput(
				"https://github.com/example/service.git",
				testUserID,
				"other-app",
				"artifact",
			),
			want: submission.ErrForbidden,
		},
		{
			name:      "repository",
			principal: identityPrincipal,
			raw: testInput(
				"https://github.com/other/private.git",
				testUserID,
				testAppID,
				"artifact",
			),
			want: submission.ErrForbidden,
		},
		{
			name: "publication action",
			principal: func(principal submission.Principal) submission.Principal {
				principal.Actions[submission.ActionSubmitPullRequest] = false
				return principal
			},
			raw: testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"pull_request",
			),
			want: submission.ErrForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestService(t)
			principal := test.principal(testPrincipal())
			_, _, err := fixture.service.Submit(
				context.Background(),
				principal,
				testSubmitRequest(test.raw),
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("Submit() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSubmitStrictValidationAndSafeErrors(t *testing.T) {
	validInput := testInput(
		"https://github.com/example/service.git",
		testUserID,
		testAppID,
		"artifact",
	)
	tests := []struct {
		name      string
		principal func(submission.Principal) submission.Principal
		request   func(submission.SubmitRequest) submission.SubmitRequest
		marker    string
		wantCause string
	}{
		{
			name:      "missing idempotency key",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.IdempotencyKey = ""
				return request
			},
		},
		{
			name:      "short idempotency key",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.IdempotencyKey = "too-short"
				return request
			},
		},
		{
			name:      "oversized idempotency key",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.IdempotencyKey = strings.Repeat("a", 129)
				return request
			},
		},
		{
			name:      "unknown template",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.Template = template.ID{Name: "other", Version: 1}
				return request
			},
		},
		{
			name:      "unknown input field",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.Input = append(
					json.RawMessage(nil),
					[]byte(`{"task_description":"secret-marker","repository_uri":"https://github.com/example/service.git","base_ref":"main","tags":{"user_id":"codex@example.com","app_id":"service"},"profile":"generic","checks":[{"name":"test","directory":".","executable":"npm","args":["test"],"timeout":"10m","required":true}],"publication":{"mode":"artifact"},"unknown":"secret-marker"}`)...,
				)
				return request
			},
			marker: "secret-marker",
		},
		{
			name: "unsupported harness",
			principal: func(principal submission.Principal) submission.Principal {
				principal.Harnesses = map[string]bool{"other": true}
				return principal
			},
			request: identityRequest,
		},
		{
			name:      "blank session",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.Origin.SessionID = " "
				return request
			},
		},
		{
			name:      "blank turn",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.Origin.TurnID = " "
				return request
			},
		},
		{
			name:      "generic profile without checks",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.Input = json.RawMessage(`{
					"task_description":"change timeout",
					"repository_uri":"https://github.com/example/service.git",
					"base_ref":"main",
					"tags":{"user_id":"codex@example.com","app_id":"service"},
					"worker_profile":"codex-go@1",
					"profile":"generic",
					"publication":{"mode":"artifact"}
				}`)
				return request
			},
			wantCause: "generic profile requires at least one check",
		},
		{
			name:      "shell-shaped check",
			principal: identityPrincipal,
			request: func(request submission.SubmitRequest) submission.SubmitRequest {
				request.Input = json.RawMessage(`{
					"task_description":"change timeout",
					"repository_uri":"https://github.com/example/service.git",
					"base_ref":"main",
					"tags":{"user_id":"codex@example.com","app_id":"service"},
					"worker_profile":"codex-go@1",
					"profile":"generic",
					"checks":[{
						"name":"test","directory":".","executable":"sh",
						"args":["-c","echo secret-marker"],"timeout":"10m","required":true
					}],
					"publication":{"mode":"artifact"}
				}`)
				return request
			},
			marker:    "secret-marker",
			wantCause: "executable must be one shell-free program",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestService(t)
			principal := test.principal(testPrincipal())
			request := test.request(testSubmitRequest(validInput))
			_, _, err := fixture.service.Submit(context.Background(), principal, request)
			if !errors.Is(err, submission.ErrInvalidRequest) &&
				!errors.Is(err, submission.ErrForbidden) {
				t.Fatalf("Submit() error = %v, want invalid request or forbidden", err)
			}
			if test.marker != "" && strings.Contains(err.Error(), test.marker) {
				t.Fatalf("Submit() error leaked %q: %v", test.marker, err)
			}
			if test.wantCause != "" && !errorTreeContains(err, test.wantCause) {
				t.Fatalf("Submit() error tree = %v, want cause containing %q", err, test.wantCause)
			}
		})
	}
}

func errorTreeContains(err error, marker string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), marker) {
		return true
	}
	switch unwrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range unwrapped.Unwrap() {
			if errorTreeContains(cause, marker) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return errorTreeContains(unwrapped.Unwrap(), marker)
	}
	return false
}

func TestSubmitCanonicalIdempotencyBinding(t *testing.T) {
	t.Run("exact normalized reuse", func(t *testing.T) {
		fixture := newTestService(t)
		first, reused, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil || reused {
			t.Fatalf("first Submit() = (%+v, %v, %v)", first, reused, err)
		}
		secondRequest := testSubmitRequest(json.RawMessage(`{
			"publication":{"mode":"artifact"},
			"checks":[{
				"required":true,"timeout":"10m","args":["test"],
				"executable":"npm","directory":".","name":"test"
			}],
			"worker_profile":"codex-go@1",
			"profile":"generic",
			"tags":{"app_id":"service","user_id":"codex@example.com"},
			"base_ref":"main",
			"repository_uri":"https://GITHUB.com/example/service",
			"task_description":"change timeout"
		}`))
		second, reused, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			secondRequest,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reused {
			t.Fatal("second Submit() reused = false, want true")
		}
		if second.Record.RunID != first.Record.RunID ||
			second.Record.RequestDigest != first.Record.RequestDigest {
			t.Fatalf(
				"second binding = %q/%q, want %q/%q",
				second.Record.RunID,
				second.Record.RequestDigest,
				first.Record.RunID,
				first.Record.RequestDigest,
			)
		}
		requests := fixture.trigger.StartRequests()
		if len(requests) != 1 {
			t.Fatalf("trigger start requests = %d, want 1", len(requests))
		}
	})

	t.Run("changed input conflicts", func(t *testing.T) {
		fixture := newTestService(t)
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		if _, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		); err != nil {
			t.Fatal(err)
		}
		var changed templatecodechange.Input
		if err := json.Unmarshal(request.Input, &changed); err != nil {
			t.Fatal(err)
		}
		changed.TaskDescription = "change a different timeout"
		request.Input, _ = json.Marshal(changed)
		_, _, err := fixture.service.Submit(context.Background(), testPrincipal(), request)
		if !errors.Is(err, submission.ErrIdempotencyConflict) {
			t.Fatalf("changed Submit() error = %v, want idempotency conflict", err)
		}
	})

	t.Run("reuse ignores a later request time", func(t *testing.T) {
		registry, err := template.NewRegistry(templatecodechange.Definition{})
		if err != nil {
			t.Fatal(err)
		}
		store := submissionmock.NewStore()
		trigger := submissionmock.NewTrigger()
		now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
		service, err := submission.New(submission.Dependencies{
			Templates: registry,
			Store:     store,
			Trigger:   trigger,
			Clock: func() time.Time {
				current := now
				now = now.Add(time.Minute)
				return current
			},
			SystemMaxDepth: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		first, reused, err := service.Submit(context.Background(), testPrincipal(), request)
		if err != nil || reused {
			t.Fatalf("first Submit() = (%+v, %v, %v)", first, reused, err)
		}
		second, reused, err := service.Submit(context.Background(), testPrincipal(), request)
		if err != nil {
			t.Fatal(err)
		}
		if !reused {
			t.Fatal("second Submit() reused = false, want true")
		}
		if !second.Record.CreatedAt.Equal(first.Record.CreatedAt) {
			t.Fatalf(
				"reused creation time = %s, want %s",
				second.Record.CreatedAt,
				first.Record.CreatedAt,
			)
		}
	})

	t.Run("changed origin conflicts", func(t *testing.T) {
		fixture := newTestService(t)
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		if _, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		); err != nil {
			t.Fatal(err)
		}
		request.Origin.TurnID = "turn-2"
		_, _, err := fixture.service.Submit(context.Background(), testPrincipal(), request)
		if !errors.Is(err, submission.ErrIdempotencyConflict) {
			t.Fatalf("changed Submit() error = %v, want idempotency conflict", err)
		}
	})

	t.Run("run identity is deterministic and credential scoped", func(t *testing.T) {
		fixture := newTestService(t)
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		first, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if err != nil {
			t.Fatal(err)
		}
		if first.Record.RunID != "paje_jgp7gvc66wvox4oxmlbn4lzwl4" {
			t.Fatalf("run ID = %q, want deterministic literal", first.Record.RunID)
		}
		other := testPrincipal()
		other.CredentialID = "cred-other-service"
		second, _, err := fixture.service.Submit(context.Background(), other, request)
		if err != nil {
			t.Fatal(err)
		}
		if second.Record.RunID == first.Record.RunID ||
			second.Record.IdempotencyKeyDigest == first.Record.IdempotencyKeyDigest {
			t.Fatalf("credential-scoped identities collided: %+v / %+v", first.Record, second.Record)
		}
	})

	t.Run("explicit zero memory limit survives canonical binding", func(t *testing.T) {
		fixture := newTestService(t)
		raw := json.RawMessage(`{
			"task_description":"change timeout",
			"repository_uri":"https://github.com/example/service.git",
			"base_ref":"main",
			"memory_limit":0,
			"tags":{"user_id":"codex@example.com","app_id":"service"},
			"worker_profile":"codex-go@1",
			"profile":"generic",
			"checks":[{
				"name":"test","directory":".","executable":"npm",
				"args":["test"],"timeout":"10m","required":true
			}],
			"publication":{"mode":"artifact"}
		}`)
		view, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(raw),
		)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := templatecodechange.Decode(view.Record.CanonicalInput)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.MemoryLimit != 0 {
			t.Fatalf("memory limit = %d, want explicit zero", decoded.MemoryLimit)
		}
	})
}

func TestSubmitConcurrentReuseStartsTriggerOnce(t *testing.T) {
	fixture := newTestService(t)
	request := testSubmitRequest(testInput(
		"https://github.com/example/service.git",
		testUserID,
		testAppID,
		"artifact",
	))

	const callers = 32
	start := make(chan struct{})
	results := make(chan struct {
		runID  string
		reused bool
		err    error
	}, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			view, reused, err := fixture.service.Submit(
				context.Background(),
				testPrincipal(),
				request,
			)
			results <- struct {
				runID  string
				reused bool
				err    error
			}{runID: view.Record.RunID, reused: reused, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	firstReservations := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.runID != "paje_jgp7gvc66wvox4oxmlbn4lzwl4" {
			t.Fatalf("run ID = %q, want deterministic literal", result.runID)
		}
		if !result.reused {
			firstReservations++
		}
	}
	if firstReservations != 1 {
		t.Fatalf("first reservations = %d, want 1", firstReservations)
	}
	if requests := fixture.trigger.StartRequests(); len(requests) != 1 {
		t.Fatalf("trigger start requests = %d, want 1", len(requests))
	}
}

func TestSubmitComputesAndEnforcesLineage(t *testing.T) {
	t.Run("allowed child binds parent root and exact depth", func(t *testing.T) {
		fixture := newTestService(t)
		principal := testPrincipal()
		principal.MaxDepth = 1
		root, _, err := fixture.service.Submit(
			context.Background(),
			principal,
			testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil {
			t.Fatal(err)
		}
		childRequest := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		childRequest.IdempotencyKey = strings.Repeat("b", 32)
		childRequest.Origin.ParentRunID = root.Record.RunID
		child, _, err := fixture.service.Submit(
			context.Background(),
			principal,
			childRequest,
		)
		if err != nil {
			t.Fatal(err)
		}
		if child.Record.RootRunID != root.Record.RunID || child.Record.Depth != 1 {
			t.Fatalf(
				"child root/depth = %q/%d, want %q/1",
				child.Record.RootRunID,
				child.Record.Depth,
				root.Record.RunID,
			)
		}
	})

	t.Run("root-only principal rejects child", func(t *testing.T) {
		fixture := newTestService(t)
		root, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil {
			t.Fatal(err)
		}
		childRequest := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		childRequest.IdempotencyKey = strings.Repeat("b", 32)
		childRequest.Origin.ParentRunID = root.Record.RunID
		_, _, err = fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			childRequest,
		)
		if !errors.Is(err, submission.ErrDepthExceeded) {
			t.Fatalf("child Submit() error = %v, want depth exceeded", err)
		}
	})

	t.Run("system maximum rejects grandchild", func(t *testing.T) {
		fixture := newTestService(t)
		principal := testPrincipal()
		principal.MaxDepth = 1
		rootRequest := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		root, _, err := fixture.service.Submit(context.Background(), principal, rootRequest)
		if err != nil {
			t.Fatal(err)
		}
		childRequest := rootRequest
		childRequest.IdempotencyKey = strings.Repeat("b", 32)
		childRequest.Origin.ParentRunID = root.Record.RunID
		child, _, err := fixture.service.Submit(context.Background(), principal, childRequest)
		if err != nil {
			t.Fatal(err)
		}
		grandchildRequest := rootRequest
		grandchildRequest.IdempotencyKey = strings.Repeat("c", 32)
		grandchildRequest.Origin.ParentRunID = child.Record.RunID
		_, _, err = fixture.service.Submit(
			context.Background(),
			principal,
			grandchildRequest,
		)
		if !errors.Is(err, submission.ErrDepthExceeded) {
			t.Fatalf("grandchild Submit() error = %v, want depth exceeded", err)
		}
	})

	t.Run("cross-principal parent is forbidden", func(t *testing.T) {
		fixture := newTestService(t)
		root, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil {
			t.Fatal(err)
		}
		other := testPrincipal()
		other.CredentialID = "cred-other-service"
		other.MaxDepth = 1
		childRequest := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		childRequest.IdempotencyKey = strings.Repeat("b", 32)
		childRequest.Origin.ParentRunID = root.Record.RunID
		_, _, err = fixture.service.Submit(context.Background(), other, childRequest)
		if !errors.Is(err, submission.ErrForbidden) {
			t.Fatalf("cross-principal Submit() error = %v, want forbidden", err)
		}
	})

	t.Run("cross-harness parent is forbidden", func(t *testing.T) {
		fixture := newTestService(t)
		root, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil {
			t.Fatal(err)
		}
		principal := testPrincipal()
		principal.MaxDepth = 1
		principal.Harnesses["other"] = true
		childRequest := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		childRequest.IdempotencyKey = strings.Repeat("b", 32)
		childRequest.Origin.Harness = "other"
		childRequest.Origin.ParentRunID = root.Record.RunID
		_, _, err = fixture.service.Submit(context.Background(), principal, childRequest)
		if !errors.Is(err, submission.ErrForbidden) {
			t.Fatalf("cross-harness Submit() error = %v, want forbidden", err)
		}
	})

	t.Run("self parent is invalid", func(t *testing.T) {
		fixture := newTestService(t)
		principal := testPrincipal()
		principal.MaxDepth = 1
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		root, _, err := fixture.service.Submit(context.Background(), principal, request)
		if err != nil {
			t.Fatal(err)
		}
		request.Origin.ParentRunID = root.Record.RunID
		_, _, err = fixture.service.Submit(context.Background(), principal, request)
		if !errors.Is(err, submission.ErrInvalidRequest) {
			t.Fatalf("self-parent Submit() error = %v, want invalid request", err)
		}
	})

	t.Run("corrupt parent chain is invalid", func(t *testing.T) {
		fixture := newTestService(t)
		principal := testPrincipal()
		principal.MaxDepth = 1
		root := submitTestRun(t, fixture, principal)
		corrupt := root.Record
		corrupt.Depth = 1
		corrupt.RootRunID = "paje_missing_root"
		fixture.store.SetRecord(corrupt)

		childRequest := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		childRequest.IdempotencyKey = strings.Repeat("b", 32)
		childRequest.Origin.ParentRunID = corrupt.RunID
		_, _, err := fixture.service.Submit(
			context.Background(),
			principal,
			childRequest,
		)
		if !errors.Is(err, submission.ErrInvalidRequest) {
			t.Fatalf("corrupt-parent Submit() error = %v, want invalid request", err)
		}
	})
}

func TestSubmitRejectsAmbiguousJSONAndRepositoryEscapes(t *testing.T) {
	t.Run("duplicate object names", func(t *testing.T) {
		tests := []struct {
			name string
			raw  json.RawMessage
		}{
			{
				name: "top level",
				raw: json.RawMessage(`{
					"task_description":"first",
					"task_description":"second",
					"repository_uri":"https://github.com/example/service.git",
					"base_ref":"main",
					"tags":{"user_id":"codex@example.com","app_id":"service"},
					"profile":"generic",
					"checks":[{
						"name":"test","directory":".","executable":"npm",
						"args":["test"],"timeout":"10m","required":true
					}],
					"publication":{"mode":"artifact"}
				}`),
			},
			{
				name: "nested identity",
				raw: json.RawMessage(`{
					"task_description":"change timeout",
					"repository_uri":"https://github.com/example/service.git",
					"base_ref":"main",
					"tags":{
						"user_id":"codex@example.com",
						"user_id":"other@example.com",
						"app_id":"service"
					},
					"profile":"generic",
					"checks":[{
						"name":"test","directory":".","executable":"npm",
						"args":["test"],"timeout":"10m","required":true
					}],
					"publication":{"mode":"artifact"}
				}`),
			},
			{
				name: "nested publication",
				raw: json.RawMessage(`{
					"task_description":"change timeout",
					"repository_uri":"https://github.com/example/service.git",
					"base_ref":"main",
					"tags":{"user_id":"codex@example.com","app_id":"service"},
					"profile":"generic",
					"checks":[{
						"name":"test","directory":".","executable":"npm",
						"args":["test"],"timeout":"10m","required":true
					}],
					"publication":{"mode":"artifact","mode":"pull_request"}
				}`),
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				fixture := newTestService(t)
				_, _, err := fixture.service.Submit(
					context.Background(),
					testPrincipal(),
					testSubmitRequest(test.raw),
				)
				if !errors.Is(err, submission.ErrInvalidRequest) {
					t.Fatalf("Submit() error = %v, want invalid request", err)
				}
				if requests := fixture.trigger.StartRequests(); len(requests) != 0 {
					t.Fatalf("trigger requests = %d, want 0", len(requests))
				}
			})
		}
	})

	t.Run("repository URL attacks", func(t *testing.T) {
		repositories := []string{
			"http://github.com/example/service.git",
			"https://token@github.com/example/service.git",
			"https://github.com:443/example/service.git",
			"https://github.com/example/service.git?token=secret",
			"https://github.com/example/service.git#secret",
			"https://github.com/example%2fservice.git",
			"https://github.com/example/service/extra.git",
			"https://github.com/example/../service.git",
			"git@github.com:example/service.git",
		}
		for _, repository := range repositories {
			t.Run(repository, func(t *testing.T) {
				fixture := newTestService(t)
				_, _, err := fixture.service.Submit(
					context.Background(),
					testPrincipal(),
					testSubmitRequest(testInput(
						repository,
						testUserID,
						testAppID,
						"artifact",
					)),
				)
				if !errors.Is(err, submission.ErrInvalidRequest) {
					t.Fatalf("Submit() error = %v, want invalid request", err)
				}
				if requests := fixture.trigger.StartRequests(); len(requests) != 0 {
					t.Fatalf("trigger requests = %d, want 0", len(requests))
				}
			})
		}
	})
}

func TestSubmitAllowsExplicitPullRequestScope(t *testing.T) {
	fixture := newTestService(t)
	principal := testPrincipal()
	principal.Actions[submission.ActionSubmitPullRequest] = true
	view, _, err := fixture.service.Submit(
		context.Background(),
		principal,
		testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"pull_request",
		)),
	)
	if err != nil {
		t.Fatal(err)
	}
	var input templatecodechange.Input
	if err := json.Unmarshal(view.Record.CanonicalInput, &input); err != nil {
		t.Fatal(err)
	}
	if input.Publication.Mode != "pull_request" ||
		input.Publication.Provider != "github" ||
		input.Publication.TargetBranch != "main" {
		t.Fatalf("bound publication = %+v", input.Publication)
	}
}

func TestSubmitRecoversAroundTriggerBinding(t *testing.T) {
	t.Run("reservation before provider start", func(t *testing.T) {
		fixture := newTestService(t)
		fixture.trigger.SetStartError(errors.New("provider secret-marker"))
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		_, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if !errors.Is(err, submission.ErrProviderUnavailable) {
			t.Fatalf("first Submit() error = %v, want provider unavailable", err)
		}
		if strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("Submit() leaked provider diagnostic: %v", err)
		}
		reserved, err := fixture.store.LoadByKey(
			context.Background(),
			testCredentialID,
			request.IdempotencyKey,
		)
		if err != nil {
			t.Fatal(err)
		}
		if reserved.Trigger != nil {
			t.Fatalf("reserved trigger = %+v, want nil", reserved.Trigger)
		}
		fixture.trigger.SetStartError(nil)
		recovered, reused, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reused || recovered.Record.Trigger == nil {
			t.Fatalf("recovered Submit() = (%+v, %v), want reused binding", recovered, reused)
		}
	})

	t.Run("provider start before durable trigger binding", func(t *testing.T) {
		fixture := newTestService(t)
		fixture.store.SetBindTriggerError(errors.New("store secret-marker"))
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		_, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if !errors.Is(err, submission.ErrProviderUnavailable) {
			t.Fatalf("first Submit() error = %v, want provider unavailable", err)
		}
		if strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("Submit() leaked store diagnostic: %v", err)
		}
		if requests := fixture.trigger.StartRequests(); len(requests) != 1 {
			t.Fatalf("trigger start requests = %d, want 1", len(requests))
		}
		fixture.store.SetBindTriggerError(nil)
		recovered, reused, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reused || recovered.Record.Trigger == nil {
			t.Fatalf("recovered Submit() = (%+v, %v), want reused binding", recovered, reused)
		}
		if requests := fixture.trigger.StartRequests(); len(requests) != 1 {
			t.Fatalf("trigger start requests after recovery = %d, want 1", len(requests))
		}
	})

	t.Run("corrupt existing trigger reference fails closed", func(t *testing.T) {
		fixture := newTestService(t)
		request := testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		))
		submitted, _, err := fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if err != nil {
			t.Fatal(err)
		}
		corrupt := submitted.Record
		corrupt.Trigger = &submission.TriggerReference{
			Provider:      "",
			ExternalRunID: submitted.Record.Trigger.ExternalRunID,
		}
		fixture.store.SetRecord(corrupt)
		_, _, err = fixture.service.Submit(
			context.Background(),
			testPrincipal(),
			request,
		)
		if !errors.Is(err, submission.ErrProviderUnavailable) {
			t.Fatalf("reused Submit() error = %v, want provider unavailable", err)
		}
	})
}

func TestMockTriggerStartReconcilesExactBinding(t *testing.T) {
	t.Run("exact retry returns the original reference", func(t *testing.T) {
		trigger := submissionmock.NewTrigger()
		request := submission.TriggerRequest{
			RunID: "paje_restart_safe",
			Input: json.RawMessage(`{"task":"one"}`),
		}
		first, err := trigger.Start(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		trigger.SetStartError(errors.New("new starts unavailable"))
		second, err := trigger.Start(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if second != first {
			t.Fatalf("second reference = %+v, want %+v", second, first)
		}
		if requests := trigger.StartRequests(); len(requests) != 1 {
			t.Fatalf("unique starts = %d, want 1", len(requests))
		}
	})

	t.Run("changed canonical input conflicts", func(t *testing.T) {
		trigger := submissionmock.NewTrigger()
		first := submission.TriggerRequest{
			RunID: "paje_restart_safe",
			Input: json.RawMessage(`{"task":"one"}`),
		}
		if _, err := trigger.Start(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		changed := first
		changed.Input = json.RawMessage(`{"task":"two"}`)
		_, err := trigger.Start(context.Background(), changed)
		if !errors.Is(err, submission.ErrIdempotencyConflict) {
			t.Fatalf("changed Start() error = %v, want idempotency conflict", err)
		}
		if requests := trigger.StartRequests(); len(requests) != 1 {
			t.Fatalf("unique starts = %d, want 1", len(requests))
		}
	})
}

func TestNewRejectsInvalidSubmissionDependencies(t *testing.T) {
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	valid := submission.Dependencies{
		Templates:      registry,
		Store:          submissionmock.NewStore(),
		Trigger:        submissionmock.NewTrigger(),
		Clock:          time.Now,
		SystemMaxDepth: 1,
	}
	tests := []struct {
		name   string
		mutate func(*submission.Dependencies)
	}{
		{
			name: "templates",
			mutate: func(dependencies *submission.Dependencies) {
				dependencies.Templates = nil
			},
		},
		{
			name: "store",
			mutate: func(dependencies *submission.Dependencies) {
				dependencies.Store = nil
			},
		},
		{
			name: "trigger",
			mutate: func(dependencies *submission.Dependencies) {
				dependencies.Trigger = nil
			},
		},
		{
			name: "clock",
			mutate: func(dependencies *submission.Dependencies) {
				dependencies.Clock = nil
			},
		},
		{
			name: "system depth above v1 maximum",
			mutate: func(dependencies *submission.Dependencies) {
				dependencies.SystemMaxDepth = 2
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := valid
			test.mutate(&dependencies)
			if _, err := submission.New(dependencies); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}

func TestInspectIsPrincipalScopedAndValidatesTerminalBinding(t *testing.T) {
	t.Run("projects running state for the owner", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		fixture.trigger.SetState(*submitted.Record.Trigger, submission.TriggerState{
			Status: submission.StatusRunning,
		})

		view, err := fixture.service.Inspect(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != submission.StatusRunning || view.Result != nil {
			t.Fatalf("Inspect() = %+v, want running without result", view)
		}
	})

	t.Run("requires read action", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		principal := testPrincipal()
		delete(principal.Actions, submission.ActionRead)
		_, err := fixture.service.Inspect(
			context.Background(),
			principal,
			submitted.Record.RunID,
		)
		if !errors.Is(err, submission.ErrForbidden) {
			t.Fatalf("Inspect() error = %v, want forbidden", err)
		}
	})

	t.Run("hides another principal record", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		principal := testPrincipal()
		principal.CredentialID = "cred-other-service"
		_, err := fixture.service.Inspect(
			context.Background(),
			principal,
			submitted.Record.RunID,
		)
		if !errors.Is(err, submission.ErrNotFound) {
			t.Fatalf("Inspect() error = %v, want not found", err)
		}
	})

	t.Run("accepts an exactly bound terminal result", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		fixture.trigger.SetState(*submitted.Record.Trigger, submission.TriggerState{
			Status: submission.StatusSucceeded,
			Result: &templatecodechange.Result{
				RunID:  submitted.Record.RunID,
				Status: run.StatusSucceeded,
			},
		})
		view, err := fixture.service.Inspect(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status != submission.StatusSucceeded ||
			view.Result == nil ||
			view.Result.RunID != submitted.Record.RunID {
			t.Fatalf("Inspect() = %+v, want bound success", view)
		}
	})

	tests := []struct {
		name  string
		state func(string) submission.TriggerState
	}{
		{
			name: "terminal result missing",
			state: func(string) submission.TriggerState {
				return submission.TriggerState{Status: submission.StatusSucceeded}
			},
		},
		{
			name: "terminal run ID mismatch",
			state: func(string) submission.TriggerState {
				return submission.TriggerState{
					Status: submission.StatusSucceeded,
					Result: &templatecodechange.Result{
						RunID:  "paje_other",
						Status: run.StatusSucceeded,
					},
				}
			},
		},
		{
			name: "terminal status mismatch",
			state: func(runID string) submission.TriggerState {
				return submission.TriggerState{
					Status: submission.StatusSucceeded,
					Result: &templatecodechange.Result{
						RunID:  runID,
						Status: run.StatusFailed,
					},
				}
			},
		},
		{
			name: "nonterminal result present",
			state: func(runID string) submission.TriggerState {
				return submission.TriggerState{
					Status: submission.StatusRunning,
					Result: &templatecodechange.Result{
						RunID:  runID,
						Status: run.StatusSucceeded,
					},
				}
			},
		},
		{
			name: "unknown provider status",
			state: func(string) submission.TriggerState {
				return submission.TriggerState{Status: submission.Status("provider_magic")}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestService(t)
			submitted := submitTestRun(t, fixture, testPrincipal())
			fixture.trigger.SetState(
				*submitted.Record.Trigger,
				test.state(submitted.Record.RunID),
			)
			_, err := fixture.service.Inspect(
				context.Background(),
				testPrincipal(),
				submitted.Record.RunID,
			)
			if !errors.Is(err, submission.ErrProviderUnavailable) {
				t.Fatalf("Inspect() error = %v, want provider unavailable", err)
			}
		})
	}
}

func TestInspectRejectsCorruptRequestDigestBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *submission.Record)
	}{
		{
			name: "origin changed without digest",
			mutate: func(_ *testing.T, record *submission.Record) {
				record.Origin.TurnID = "turn-corrupt"
			},
		},
		{
			name: "canonical input changed without digest",
			mutate: func(t *testing.T, record *submission.Record) {
				t.Helper()
				var input templatecodechange.Input
				if err := json.Unmarshal(record.CanonicalInput, &input); err != nil {
					t.Fatal(err)
				}
				input.TaskDescription = "corrupt durable task"
				encoded, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				record.CanonicalInput, err = run.CanonicalInput(encoded)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "digest changed to another valid hash",
			mutate: func(_ *testing.T, record *submission.Record) {
				record.RequestDigest = strings.Repeat("f", 64)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestService(t)
			submitted := submitTestRun(t, fixture, testPrincipal())
			corrupt := submitted.Record
			test.mutate(t, &corrupt)
			fixture.store.SetRecord(corrupt)

			_, err := fixture.service.Inspect(
				context.Background(),
				testPrincipal(),
				submitted.Record.RunID,
			)
			if !errors.Is(err, submission.ErrProviderUnavailable) {
				t.Fatalf("Inspect() error = %v, want provider unavailable", err)
			}
		})
	}
}

func TestInspectAndCancelRejectMisindexedStoreLoad(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *submission.Service, submission.Principal, string) error
	}{
		{
			name: "inspect",
			call: func(
				ctx context.Context,
				service *submission.Service,
				principal submission.Principal,
				runID string,
			) error {
				_, err := service.Inspect(ctx, principal, runID)
				return err
			},
		},
		{
			name: "cancel",
			call: func(
				ctx context.Context,
				service *submission.Service,
				principal submission.Principal,
				runID string,
			) error {
				_, _, err := service.Cancel(ctx, principal, runID)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, trigger := newMisindexedService(t)
			principal := testPrincipal()
			firstRequest := testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			))
			first, _, err := service.Submit(
				context.Background(),
				principal,
				firstRequest,
			)
			if err != nil {
				t.Fatal(err)
			}
			secondRequest := firstRequest
			secondRequest.IdempotencyKey = strings.Repeat("b", 32)
			second, _, err := service.Submit(
				context.Background(),
				principal,
				secondRequest,
			)
			if err != nil {
				t.Fatal(err)
			}
			store.ReturnOnLoad(second.Record)

			err = test.call(context.Background(), service, principal, first.Record.RunID)
			if !errors.Is(err, submission.ErrProviderUnavailable) {
				t.Fatalf("operation error = %v, want provider unavailable", err)
			}
			if calls := trigger.CancelCalls(*second.Record.Trigger); calls != 0 {
				t.Fatalf("misindexed provider cancel calls = %d, want 0", calls)
			}
		})
	}
}

func TestCancelRecordsIntentOnceAndPreservesTerminalState(t *testing.T) {
	t.Run("records intent before one provider action", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		reference := *submitted.Record.Trigger
		fixture.trigger.SetState(reference, submission.TriggerState{
			Status: submission.StatusRunning,
		})

		first, newlyRequested, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !newlyRequested || first.Status != submission.StatusCancellationRequested ||
			first.Record.CancellationRequested == nil {
			t.Fatalf("first Cancel() = %+v, want cancellation requested", first)
		}
		second, newlyRequested, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if newlyRequested || second.Status != submission.StatusCancellationRequested {
			t.Fatalf("second Cancel() status = %q", second.Status)
		}
		if calls := fixture.trigger.CancelCalls(reference); calls != 1 {
			t.Fatalf("provider cancel calls = %d, want 1", calls)
		}
		inspected, err := fixture.service.Inspect(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Status != submission.StatusCancellationRequested {
			t.Fatalf("Inspect() status = %q, want cancellation requested", inspected.Status)
		}
	})

	t.Run("terminal run is returned without cancellation", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		reference := *submitted.Record.Trigger
		fixture.trigger.SetState(reference, submission.TriggerState{
			Status: submission.StatusSucceeded,
			Result: &templatecodechange.Result{
				RunID:  submitted.Record.RunID,
				Status: run.StatusSucceeded,
			},
		})
		view, newlyRequested, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if newlyRequested || view.Status != submission.StatusSucceeded ||
			view.Record.CancellationRequested != nil {
			t.Fatalf("Cancel() = %+v, want unchanged terminal success", view)
		}
		if calls := fixture.trigger.CancelCalls(reference); calls != 0 {
			t.Fatalf("provider cancel calls = %d, want 0", calls)
		}
	})

	t.Run("requires cancel action", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		principal := testPrincipal()
		delete(principal.Actions, submission.ActionCancel)
		_, _, err := fixture.service.Cancel(
			context.Background(),
			principal,
			submitted.Record.RunID,
		)
		if !errors.Is(err, submission.ErrForbidden) {
			t.Fatalf("Cancel() error = %v, want forbidden", err)
		}
	})

	t.Run("hides another principal record", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		principal := testPrincipal()
		principal.CredentialID = "cred-other-service"
		_, _, err := fixture.service.Cancel(
			context.Background(),
			principal,
			submitted.Record.RunID,
		)
		if !errors.Is(err, submission.ErrNotFound) {
			t.Fatalf("Cancel() error = %v, want not found", err)
		}
	})

	t.Run("ambiguous provider cancellation is never repeated", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		reference := *submitted.Record.Trigger
		fixture.trigger.SetState(reference, submission.TriggerState{
			Status: submission.StatusRunning,
		})
		fixture.trigger.SetCancelError(errors.New("provider secret-marker"))
		_, _, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if !errors.Is(err, submission.ErrProviderUnavailable) {
			t.Fatalf("first Cancel() error = %v, want provider unavailable", err)
		}
		if strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("Cancel() leaked provider diagnostic: %v", err)
		}
		fixture.trigger.SetCancelError(nil)
		second, newlyRequested, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if newlyRequested {
			t.Fatal("second Cancel() newly requested = true, want false")
		}
		if second.Status != submission.StatusCancellationRequested {
			t.Fatalf("second Cancel() status = %q", second.Status)
		}
		if calls := fixture.trigger.CancelCalls(reference); calls != 1 {
			t.Fatalf("provider cancel calls = %d, want no ambiguous retry", calls)
		}
	})

	t.Run("durable repeat survives unavailable provider inspection", func(t *testing.T) {
		fixture := newTestService(t)
		submitted := submitTestRun(t, fixture, testPrincipal())
		reference := *submitted.Record.Trigger
		fixture.trigger.SetState(reference, submission.TriggerState{
			Status: submission.StatusRunning,
		})
		if _, _, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		); err != nil {
			t.Fatal(err)
		}
		fixture.trigger.SetInspectError(errors.New("provider unavailable"))
		repeated, newlyRequested, err := fixture.service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if newlyRequested {
			t.Fatal("repeated Cancel() newly requested = true, want false")
		}
		if repeated.Status != submission.StatusCancellationRequested {
			t.Fatalf("repeated Cancel() status = %q", repeated.Status)
		}
		if calls := fixture.trigger.CancelCalls(reference); calls != 1 {
			t.Fatalf("provider cancel calls = %d, want 1", calls)
		}
	})

	t.Run("corrupt cancellation persistence fails closed", func(t *testing.T) {
		registry, err := template.NewRegistry(templatecodechange.Definition{})
		if err != nil {
			t.Fatal(err)
		}
		store := submissionmock.NewStore()
		trigger := submissionmock.NewTrigger()
		service, err := submission.New(submission.Dependencies{
			Templates: registry,
			Store: corruptCancellationStore{
				Store: store,
			},
			Trigger:        trigger,
			Clock:          func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
			SystemMaxDepth: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		submitted, _, err := service.Submit(
			context.Background(),
			testPrincipal(),
			testSubmitRequest(testInput(
				"https://github.com/example/service.git",
				testUserID,
				testAppID,
				"artifact",
			)),
		)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = service.Cancel(
			context.Background(),
			testPrincipal(),
			submitted.Record.RunID,
		)
		if !errors.Is(err, submission.ErrProviderUnavailable) {
			t.Fatalf("Cancel() error = %v, want provider unavailable", err)
		}
	})
}

func submitTestRun(
	t *testing.T,
	fixture serviceFixture,
	principal submission.Principal,
) submission.View {
	t.Helper()
	view, reused, err := fixture.service.Submit(
		context.Background(),
		principal,
		testSubmitRequest(testInput(
			"https://github.com/example/service.git",
			testUserID,
			testAppID,
			"artifact",
		)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("Submit() reused = true, want false")
	}
	if view.Record.Trigger == nil {
		t.Fatal("Submit() trigger = nil")
	}
	return view
}

func identityPrincipal(principal submission.Principal) submission.Principal {
	return principal
}

func identityRequest(request submission.SubmitRequest) submission.SubmitRequest {
	return request
}

type corruptCancellationStore struct {
	submission.Store
}

func (s corruptCancellationStore) MarkCancellationRequested(
	ctx context.Context,
	runID string,
	at time.Time,
) (submission.Record, bool, error) {
	record, newlyRequested, err := s.Store.MarkCancellationRequested(ctx, runID, at)
	if err != nil {
		return submission.Record{}, false, err
	}
	record.Trigger = nil
	return record, newlyRequested, nil
}

type misindexedStore struct {
	submission.Store
	mu     sync.Mutex
	record *submission.Record
}

func (s *misindexedStore) Load(
	ctx context.Context,
	runID string,
) (submission.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record != nil {
		return *s.record, nil
	}
	return s.Store.Load(ctx, runID)
}

func (s *misindexedStore) ReturnOnLoad(record submission.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := record
	cloned.CanonicalInput = append(json.RawMessage(nil), record.CanonicalInput...)
	if record.Trigger != nil {
		reference := *record.Trigger
		cloned.Trigger = &reference
	}
	s.record = &cloned
}

func newMisindexedService(
	t *testing.T,
) (*submission.Service, *misindexedStore, *submissionmock.Trigger) {
	t.Helper()
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	store := &misindexedStore{Store: submissionmock.NewStore()}
	trigger := submissionmock.NewTrigger()
	service, err := submission.New(submission.Dependencies{
		Templates:      registry,
		Store:          store,
		Trigger:        trigger,
		Clock:          func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
		SystemMaxDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, trigger
}

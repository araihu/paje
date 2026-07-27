package admission

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestJournalPersistedAdmissionOutcomeRejectsSemanticRebinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*outcomeEnvelope)
	}{
		{"control run", func(outcome *outcomeEnvelope) { outcome.Admission.ControlRunID = "run-b" }},
		{"subject", func(outcome *outcomeEnvelope) { outcome.Admission.Subject.WorkID = "other-work" }},
		{"state", func(outcome *outcomeEnvelope) { outcome.Admission.State = AdmissionAdmitted }},
		{"limiter", func(outcome *outcomeEnvelope) { outcome.Admission.LimitingQuota = QuotaInstallation }},
		{"sequence", func(outcome *outcomeEnvelope) { outcome.Admission.Sequence = 17 }},
		{"graph revision", func(outcome *outcomeEnvelope) { outcome.Admission.GraphRevision++ }},
		{"generation", func(outcome *outcomeEnvelope) { outcome.Admission.Generation++ }},
		{"request digest", func(outcome *outcomeEnvelope) {
			outcome.Admission.OriginalRequestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{"receipt", func(outcome *outcomeEnvelope) { outcome.Admission.ReceiptID = "forged-receipt" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := validPersistedAdmissionFixture(t)
			test.mutate(&fixture.outcome)
			fixture.refreshOutcome(t)
			if err := fixture.validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("validatePersistedTransition() error = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestJournalRejectsDeclaredSubjectDigestThatDoesNotMatchTypedBytes(t *testing.T) {
	fixture := validPersistedAdmissionFixture(t)
	changed := fixture.outcome.Admission.Subject
	changed.WorkID = "other-work"
	subjectPayload, err := canonicalValue(changed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.request.Subject = subjectPayload
	fixture.outcome.Subject = subjectPayload
	fixture.outcome.Admission.Subject = changed
	requestPayload, err := canonicalValue(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.requestPayload = requestPayload
	fixture.action.CanonicalRequestDigest = digestBytes(requestPayload)
	fixture.outcome.Admission.OriginalRequestDigest = fixture.action.CanonicalRequestDigest
	fixture.outcome.Admission.ReceiptID = opaqueReceiptID(
		OperationAdmissionReserve, fixture.outcome.Binding, fixture.action.CanonicalRequestDigest,
	)
	actionDigest, err := journal.Digest(fixture.action)
	if err != nil {
		t.Fatal(err)
	}
	fixture.reservation.PayloadDigest = actionDigest
	fixture.refreshOutcome(t)
	if err := fixture.validate(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("validatePersistedTransition(subject digest mismatch) error = %v, want ErrInvalidRecord", err)
	}
}

func TestJournalRejectsOrphanedAdmissionReservation(t *testing.T) {
	store, err := journal.NewMemoryStore("installation-a")
	if err != nil {
		t.Fatal(err)
	}
	action := journal.Action{
		ID: "admission_action_orphan", ControlRunID: "run-a", Kind: journal.KindAllocateResource,
		GraphRevision:          1,
		CanonicalRequestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		IdempotencyKey:         "orphan-key",
	}
	if _, _, err := store.Reserve(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	service, err := New(Dependencies{
		Store: store, Clock: func() time.Time { return time.Unix(100, 0).UTC() },
		Policy: Policy{Version: 1, InstallationLimit: 1, PrincipalLimit: 1, RunLimit: 1, ProjectLimit: 1, PrimitiveLimit: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Admission(context.Background(), "run-a", "missing"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Admission(orphaned reservation) error = %v, want ErrInvalidRecord", err)
	}
}

func TestJournalProjectionRejectsForgedTransitionState(t *testing.T) {
	t.Run("queue release of admitted item", func(t *testing.T) {
		p := newProjection()
		subject := AdmissionSubject{
			ID: "admission-a", PrincipalID: "principal-a", ProjectID: "project-a",
			Primitive: "persistent_session", WorkID: "work-a",
		}
		p.admissions[scopedID("run-a", subject.ID)] = RunAdmission{
			ControlRunID: "run-a", Subject: subject, State: AdmissionAdmitted,
			OriginalRequestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}
		err := p.apply(transitionRecord{
			binding: semanticBinding{ControlRunID: "run-a"},
			outcome: outcomeEnvelope{
				SemanticOperation: OperationQueueRelease,
				AdmissionTombstone: &AdmissionTombstone{
					ControlRunID: "run-a", Subject: subject,
					OriginalRequestDigest: p.admissions[scopedID("run-a", subject.ID)].OriginalRequestDigest,
					TerminalReceiptID:     "receipt-a", ReleasedAt: time.Unix(100, 0).UTC(),
				},
			},
			receipt: journal.CommitReceipt{Outcome: journal.Event{JournalPosition: 2}},
		})
		if !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("apply(queue release admitted) error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("early or expiry-rebound tombstone", func(t *testing.T) {
		p := newProjection()
		subject := LeaseSubject{
			ID: "lease-a", HolderID: "holder-a", Mode: LeaseExclusive,
			Resource: ResourceKey{Namespace: "project", ProjectID: "project-a", Name: "resource-a"},
		}
		originalDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		p.leases[scopedID("run-a", subject.ID)] = Lease{
			ControlRunID: "run-a", Subject: subject, State: LeaseActive,
			IssuedAt: time.Unix(100, 0).UTC(), ExpiresAt: time.Unix(200, 0).UTC(),
			OriginalRequestDigest: originalDigest,
		}
		requestSubject, err := canonicalValue(leaseTransitionSubject{
			Lease: subject, ExpiresAt: time.Unix(201, 0).UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		err = p.apply(transitionRecord{
			binding: semanticBinding{ControlRunID: "run-a"},
			request: requestEnvelope{Subject: requestSubject},
			outcome: outcomeEnvelope{
				SemanticOperation: OperationLeaseExpire,
				LeaseTombstone: &LeaseTombstone{
					ControlRunID: "run-a", Subject: subject, State: LeaseExpired,
					OriginalRequestDigest: originalDigest, TerminalReceiptID: "receipt-a",
					TerminalAt: time.Unix(199, 0).UTC(),
				},
			},
		})
		if !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("apply(rebound early expiry) error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("scanner apply without start", func(t *testing.T) {
		p := newProjection()
		identity := RecoveryIdentity{
			InstallationID: "installation-a", ControlRunID: "run-a", ActionID: "target-a",
			Generation:    1,
			SubjectDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		}
		err := p.apply(transitionRecord{
			binding: semanticBinding{ControlRunID: "run-a"},
			outcome: outcomeEnvelope{
				SemanticOperation: OperationScannerApply,
				Recovery:          &RecoveryRecord{Identity: identity, State: RecoveryApplied},
			},
		})
		if !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("apply(scanner without start) error = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("overlapping observation generation", func(t *testing.T) {
		p := newProjection()
		identity := RecoveryIdentity{
			InstallationID: "installation-a", ControlRunID: "run-a", ActionID: "target-a",
			Generation:    1,
			SubjectDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		}
		p.recoveries[recoveryIndex(identity)] = RecoveryRecord{
			Identity: identity, State: RecoveryObservationStarted,
		}
		identity.Generation++
		err := p.apply(transitionRecord{
			binding: semanticBinding{ControlRunID: "run-a"},
			outcome: outcomeEnvelope{
				SemanticOperation: OperationStartObservation,
				Recovery:          &RecoveryRecord{Identity: identity, State: RecoveryObservationStarted},
			},
		})
		if !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("apply(overlapping observation) error = %v, want ErrInvalidRecord", err)
		}
	})
}

func TestFenceProjectionRejectsTerminalProofToApplied(t *testing.T) {
	for _, state := range []RecoveryState{RecoveryCanceled, RecoveryNotPerformed} {
		t.Run(string(state), func(t *testing.T) {
			p := newProjection()
			identity := RecoveryIdentity{
				InstallationID: "installation-a", ControlRunID: "run-a", ActionID: "target-a",
				Generation:    1,
				SubjectDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			}
			prior := RecoveryRecord{Identity: identity, State: state, ReceiptID: "terminal-receipt"}
			p.recoveries[recoveryIndex(identity)] = prior
			err := p.apply(transitionRecord{
				binding: semanticBinding{ControlRunID: "run-a"},
				outcome: outcomeEnvelope{
					SemanticOperation: OperationScannerApply,
					Recovery: &RecoveryRecord{
						Identity: identity, State: RecoveryApplied, ReceiptID: "contradictory-receipt",
					},
				},
			})
			if !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("apply(%s to applied) error = %v, want ErrInvalidRecord", state, err)
			}
			if got := p.recoveries[recoveryIndex(identity)]; !reflect.DeepEqual(got, prior) {
				t.Fatalf("rejected transition mutated projection: got %#v, want %#v", got, prior)
			}
		})
	}
}

type persistedAdmissionFixture struct {
	installationID string
	action         journal.Action
	reservation    journal.Event
	event          journal.Event
	requestPayload []byte
	outcomePayload []byte
	request        requestEnvelope
	outcome        outcomeEnvelope
}

func validPersistedAdmissionFixture(t *testing.T) persistedAdmissionFixture {
	t.Helper()
	binding := semanticBinding{
		ActionID: "action-a", AttemptID: "attempt-a", ControlRunID: "run-a", Generation: 5,
		GraphRevision: 7, IdempotencyKey: "key-a", InstallationID: "installation-a",
		OutcomeEventID: "outcome-a", OutcomeKind: journal.EventActionResult,
		SemanticOperation: OperationAdmissionReserve, TaskID: "task-a",
	}
	subject := AdmissionSubject{
		ID: "admission-a", PrincipalID: "principal-a", ProjectID: "project-a",
		Primitive: "persistent_session", WorkID: "work-a",
	}
	subjectDigest, subjectPayload, err := digestValue(subject)
	if err != nil {
		t.Fatal(err)
	}
	binding.SubjectDigest = subjectDigest
	policy := Policy{
		Version: 1, InstallationLimit: 10, PrincipalLimit: 10, RunLimit: 10,
		ProjectLimit: 10, PrimitiveLimit: 10,
	}
	policyDigest, _, err := digestValue(policy)
	if err != nil {
		t.Fatal(err)
	}
	request := requestEnvelope{
		Binding: binding, Component: component, SchemaVersion: SchemaVersion,
		SemanticOperation: OperationAdmissionReserve, Subject: subjectPayload,
		SubjectDigest: subjectDigest, SubjectKind: "run_admission",
		PolicyVersion: policy.Version, PolicyDigest: policyDigest,
	}
	requestPayload, err := canonicalValue(request)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest := digestBytes(requestPayload)
	outcome := outcomeEnvelope{
		Admission: &RunAdmission{
			ControlRunID: "run-a", Subject: subject, State: AdmissionReserved,
			GraphRevision: binding.GraphRevision, Generation: binding.Generation,
			PolicyVersion: policy.Version, PolicyDigest: policyDigest,
			OriginalRequestDigest: requestDigest,
			ReceiptID:             opaqueReceiptID(OperationAdmissionReserve, binding, requestDigest),
		},
		Binding: binding, Component: component, SchemaVersion: SchemaVersion,
		SemanticOperation: OperationAdmissionReserve, Subject: subjectPayload,
		SubjectDigest: subjectDigest, SubjectKind: "run_admission",
		PolicyVersion: policy.Version, PolicyDigest: policyDigest,
	}
	action := journal.Action{
		ID: internalActionID(binding), ControlRunID: binding.ControlRunID, TaskID: binding.TaskID,
		AttemptID: binding.AttemptID, Kind: journal.KindAllocateResource,
		GraphRevision: binding.GraphRevision, CanonicalRequestDigest: requestDigest,
		IdempotencyKey: internalIdempotencyKey(binding),
	}
	actionDigest, err := journal.Digest(action)
	if err != nil {
		t.Fatal(err)
	}
	fixture := persistedAdmissionFixture{
		installationID: binding.InstallationID, action: action, request: request,
		requestPayload: requestPayload, outcome: outcome,
		reservation: journal.Event{
			ControlRunID: binding.ControlRunID, RunSequence: 1, JournalPosition: 1,
			ActionID: action.ID, Kind: journal.EventActionReserved, PayloadDigest: actionDigest,
		},
		event: journal.Event{
			ID: internalOutcomeID(binding), ControlRunID: binding.ControlRunID, RunSequence: 2,
			JournalPosition: 2, ActionID: action.ID, Kind: binding.OutcomeKind,
		},
	}
	fixture.refreshOutcome(t)
	if err := fixture.validate(); err != nil {
		t.Fatalf("valid fixture error = %v", err)
	}
	return fixture
}

func (f *persistedAdmissionFixture) refreshOutcome(t *testing.T) {
	t.Helper()
	payload, err := canonicalValue(f.outcome)
	if err != nil {
		t.Fatal(err)
	}
	f.outcomePayload = payload
	f.event.PayloadDigest = digestBytes(payload)
}

func (f persistedAdmissionFixture) validate() error {
	return validatePersistedTransition(
		f.installationID, f.action, f.reservation, f.event,
		f.requestPayload, f.outcomePayload, f.request, f.outcome,
	)
}

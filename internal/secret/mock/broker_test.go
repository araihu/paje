package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestBrokerRecordsAcquisitionAndRevocation(t *testing.T) {
	materialization, err := secret.NewValueMaterialization(workerprofile.DeliveryFile, "/run/paje/secrets/token", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := secret.NewLease("lease-1", time.Unix(200, 0).UTC(), materialization)
	if err != nil {
		t.Fatal(err)
	}
	broker := NewBroker()
	broker.SetAcquireResult("workload.token", lease, nil)
	request := secret.AcquireRequest{Capability: "workload.token"}
	got, err := broker.Acquire(context.Background(), request)
	if err != nil || got.ID() != lease.ID() {
		t.Fatalf("Acquire() = %q, %v", got.ID(), err)
	}
	if err := broker.Revoke(context.Background(), got.ID()); err != nil {
		t.Fatal(err)
	}
	if len(broker.Requests()) != 1 || len(broker.Revocations()) != 1 {
		t.Fatalf("requests/revocations = %#v %#v", broker.Requests(), broker.Revocations())
	}

	want := errors.New("unavailable")
	broker.SetAcquireResult("workload.token", secret.Lease{}, want)
	if _, err := broker.Acquire(context.Background(), request); !errors.Is(err, want) {
		t.Fatalf("Acquire() error = %v", err)
	}
}

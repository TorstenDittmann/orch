package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"change.codes/torsten/better-swarm/internal/model"
)

func TestJoinTokenLifecycleAndExactEnrollmentReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	createdAt := testCreatedAt.Add(time.Minute)
	create := model.CreateJoinToken{ID: "join_0123456789abcdef0123456789abcdef", NodeName: "node-1", SecretHash: strings.Repeat("a", 64),
		ExpiresAt: createdAt.Add(15 * time.Minute), IdempotencyKey: "join-create-key-one", RequestHash: strings.Repeat("1", 64), CreatedAt: createdAt}
	first, err := state.CreateJoinToken(ctx, create)
	if err != nil || !first.Changed || first.Token.State != model.JoinTokenPending || first.Token.SecretHash != "" {
		t.Fatalf("CreateJoinToken() = %#v, %v", first, err)
	}
	replay, err := state.CreateJoinToken(ctx, create)
	if err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if err := state.ValidateJoinToken(ctx, create.ID, strings.Repeat("b", 64), create.NodeName, createdAt); !errors.Is(err, ErrJoinTokenUnavailable) {
		t.Fatalf("wrong secret = %v", err)
	}
	enroll := testEnrollAgent("node-one", "node-1", strings.Repeat("c", 64), "joined-enroll-key-one", strings.Repeat("2", 64))
	enroll.CreatedAt = createdAt.Add(time.Minute)
	joined, err := state.EnrollAgentWithJoin(ctx, create.ID, create.SecretHash, enroll)
	if err != nil || !joined.Changed || joined.NodeID != "node-one" {
		t.Fatalf("EnrollAgentWithJoin() = %#v, %v", joined, err)
	}
	joinedReplay, err := state.EnrollAgentWithJoin(ctx, create.ID, create.SecretHash, enroll)
	if err != nil || !reflect.DeepEqual(joinedReplay, joined) {
		t.Fatalf("joined replay = %#v, %v", joinedReplay, err)
	}
	changed := enroll
	changed.RequestHash = strings.Repeat("3", 64)
	if _, err := state.EnrollAgentWithJoin(ctx, create.ID, create.SecretHash, changed); !errors.Is(err, ErrJoinTokenUnavailable) {
		t.Fatalf("changed replay = %v", err)
	}
	tokens, err := state.JoinTokens(ctx, createdAt.Add(2*time.Minute))
	if err != nil || len(tokens) != 1 || tokens[0].State != model.JoinTokenConsumed {
		t.Fatalf("JoinTokens() = %#v, %v", tokens, err)
	}
}

func TestJoinTokenPendingLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	createdAt := testCreatedAt.Add(time.Minute)
	for index := 0; index < maxPendingJoinTokens; index++ {
		input := model.CreateJoinToken{
			ID: fmt.Sprintf("join_%027d", index), NodeName: fmt.Sprintf("node-%d", index), SecretHash: strings.Repeat("a", 64),
			ExpiresAt: createdAt.Add(15 * time.Minute), IdempotencyKey: fmt.Sprintf("join-create-%016d", index),
			RequestHash: fmt.Sprintf("%064x", index+1), CreatedAt: createdAt,
		}
		if _, err := state.CreateJoinToken(ctx, input); err != nil {
			t.Fatalf("create token %d: %v", index, err)
		}
	}
	overflow := model.CreateJoinToken{ID: "join_999999999999999999999999999", NodeName: "node-overflow", SecretHash: strings.Repeat("b", 64),
		ExpiresAt: createdAt.Add(15 * time.Minute), IdempotencyKey: "join-create-overflow", RequestHash: strings.Repeat("f", 64), CreatedAt: createdAt}
	if _, err := state.CreateJoinToken(ctx, overflow); !errors.Is(err, ErrJoinTokenLimit) {
		t.Fatalf("pending token overflow = %v", err)
	}
}

func TestConcurrentJoinConsumptionCreatesOneNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	createdAt := testCreatedAt.Add(time.Minute)
	token := model.CreateJoinToken{ID: "join_concurrent_consumption", NodeName: "node-1", SecretHash: strings.Repeat("a", 64),
		ExpiresAt: createdAt.Add(15 * time.Minute), IdempotencyKey: "join-create-concurrent", RequestHash: strings.Repeat("1", 64), CreatedAt: createdAt}
	if _, err := state.CreateJoinToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	requests := []model.EnrollAgent{
		testEnrollAgent("node-one", "node-1", strings.Repeat("b", 64), "joined-concurrent-one", strings.Repeat("2", 64)),
		testEnrollAgent("node-two", "node-1", strings.Repeat("c", 64), "joined-concurrent-two", strings.Repeat("3", 64)),
	}
	for index := range requests {
		requests[index].CreatedAt = createdAt.Add(time.Minute)
	}
	start := make(chan struct{})
	errorsSeen := make([]error, len(requests))
	results := make([]model.EnrollAgentResult, len(requests))
	var group sync.WaitGroup
	for index := range requests {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			results[index], errorsSeen[index] = state.EnrollAgentWithJoin(ctx, token.ID, token.SecretHash, requests[index])
		}(index)
	}
	close(start)
	group.Wait()
	succeeded, rejected := 0, 0
	for index := range requests {
		if errorsSeen[index] == nil && results[index].Changed {
			succeeded++
		} else if errors.Is(errorsSeen[index], ErrJoinTokenUnavailable) {
			rejected++
		} else {
			t.Fatalf("request %d = %#v, %v", index, results[index], errorsSeen[index])
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("succeeded=%d rejected=%d", succeeded, rejected)
	}
	nodes, err := state.Nodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %#v, %v", nodes, err)
	}
}

func TestJoinTokenExpiryRevocationAndReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	create := model.CreateJoinToken{ID: "join_expired_credential", NodeName: "node-1", SecretHash: strings.Repeat("a", 64), ExpiresAt: testCreatedAt.Add(2 * time.Minute),
		IdempotencyKey: "join-create-expired", RequestHash: strings.Repeat("1", 64), CreatedAt: testCreatedAt.Add(time.Minute)}
	if _, err := state.CreateJoinToken(ctx, create); err != nil {
		t.Fatal(err)
	}
	if err := state.ValidateJoinToken(ctx, create.ID, create.SecretHash, create.NodeName, create.ExpiresAt); !errors.Is(err, ErrJoinTokenUnavailable) {
		t.Fatalf("expiry boundary = %v", err)
	}
	replacement := create
	replacement.ID = "join_replacement_token"
	replacement.IdempotencyKey = "join-create-replace"
	replacement.RequestHash = strings.Repeat("2", 64)
	replacement.CreatedAt = create.ExpiresAt
	replacement.ExpiresAt = replacement.CreatedAt.Add(15 * time.Minute)
	if _, err := state.CreateJoinToken(ctx, replacement); err != nil {
		t.Fatalf("replacement after expiry = %v", err)
	}
	revoke := model.RevokeJoinToken{ID: replacement.ID, IdempotencyKey: "join-revoke-key-one", RequestHash: strings.Repeat("3", 64), RevokedAt: replacement.CreatedAt.Add(time.Minute)}
	result, err := state.RevokeJoinToken(ctx, revoke)
	if err != nil || !result.Changed || result.Token.State != model.JoinTokenRevoked {
		t.Fatalf("RevokeJoinToken() = %#v, %v", result, err)
	}
	replay, err := state.RevokeJoinToken(ctx, revoke)
	if err != nil || !reflect.DeepEqual(replay, result) {
		t.Fatalf("revoke replay = %#v, %v", replay, err)
	}
}

func TestAgentCertificateRenewalRequiresActivation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	enroll := testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))
	if _, err := state.EnrollAgent(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	renew := model.RenewAgentCertificate{NodeID: enroll.NodeID, CurrentSerial: enroll.CertificateSerial, PublicKeyHash: enroll.PublicKeyHash,
		CertificateSerial: "29", CertificatePEM: []byte("renewed certificate"), CertificateNotAfter: testCreatedAt.AddDate(2, 0, 0),
		IdempotencyKey: "renew-certificate-one", RequestHash: strings.Repeat("2", 64), CreatedAt: testCreatedAt.Add(time.Hour)}
	issued, err := state.IssueAgentCertificateRenewal(ctx, renew)
	if err != nil || !issued.Changed {
		t.Fatalf("IssueAgentCertificateRenewal() = %#v, %v", issued, err)
	}
	if slot, err := state.AgentCertificateSlot(ctx, enroll.NodeID, enroll.CertificateSerial, enroll.PublicKeyHash); err != nil || slot != "CURRENT" {
		t.Fatalf("current slot = %q, %v", slot, err)
	}
	if slot, err := state.AgentCertificateSlot(ctx, enroll.NodeID, renew.CertificateSerial, enroll.PublicKeyHash); err != nil || slot != "PENDING" {
		t.Fatalf("pending slot = %q, %v", slot, err)
	}
	if _, err := state.IssueAgentCertificateRenewal(ctx, model.RenewAgentCertificate{NodeID: enroll.NodeID, CurrentSerial: enroll.CertificateSerial, PublicKeyHash: enroll.PublicKeyHash, CertificateSerial: "31", CertificatePEM: []byte("other"), CertificateNotAfter: renew.CertificateNotAfter, IdempotencyKey: "renew-certificate-two", RequestHash: strings.Repeat("3", 64), CreatedAt: renew.CreatedAt}); !errors.Is(err, ErrCertificateRenewalPending) {
		t.Fatalf("second pending = %v", err)
	}
	promoted, err := state.PromoteAgentCertificate(ctx, enroll.NodeID, renew.CertificateSerial, renew.CreatedAt.Add(time.Minute))
	if err != nil || !promoted {
		t.Fatalf("PromoteAgentCertificate() = %v, %v", promoted, err)
	}
	node, err := state.NodeByID(ctx, enroll.NodeID)
	if err != nil || node.CertificateSerial != renew.CertificateSerial || node.PendingCertificateSerial != "" {
		t.Fatalf("promoted node = %#v, %v", node, err)
	}
	if slot, err := state.AgentCertificateSlot(ctx, enroll.NodeID, enroll.CertificateSerial, enroll.PublicKeyHash); err == nil || slot != "" {
		t.Fatalf("retired slot = %q, %v", slot, err)
	}
}

func TestConcurrentCertificatePromotionIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := openTestStore(t)
	if err := state.InitializeCluster(ctx, testCluster("cluster-one")); err != nil {
		t.Fatal(err)
	}
	enroll := testEnrollAgent("node-one", "node-1", strings.Repeat("a", 64), "enrollment-key-one", strings.Repeat("1", 64))
	if _, err := state.EnrollAgent(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	renew := model.RenewAgentCertificate{NodeID: enroll.NodeID, CurrentSerial: enroll.CertificateSerial, PublicKeyHash: enroll.PublicKeyHash,
		CertificateSerial: "29", CertificatePEM: []byte("renewed certificate"), CertificateNotAfter: testCreatedAt.AddDate(2, 0, 0),
		IdempotencyKey: "renew-certificate-one", RequestHash: strings.Repeat("2", 64), CreatedAt: testCreatedAt.Add(time.Hour)}
	if _, err := state.IssueAgentCertificateRenewal(ctx, renew); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	changed := make([]bool, 2)
	errorsSeen := make([]error, 2)
	var group sync.WaitGroup
	for index := range changed {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			changed[index], errorsSeen[index] = state.PromoteAgentCertificate(ctx, enroll.NodeID, renew.CertificateSerial, renew.CreatedAt.Add(time.Minute))
		}(index)
	}
	close(start)
	group.Wait()
	if errorsSeen[0] != nil || errorsSeen[1] != nil || changed[0] == changed[1] {
		t.Fatalf("promotion results changed=%v errors=%v", changed, errorsSeen)
	}
}

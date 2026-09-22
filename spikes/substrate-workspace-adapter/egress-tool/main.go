// Creates/updates an actor's EgressPolicy directly via gRPC, since kubectl-ate has no CLI verb
// for it as of the pinned commit (confirmed by the substrate checkout's own
// demos/egress/README.md: "test-egress.sh creates and resumes the Actor but cannot create its
// EgressPolicy (no CLI verb yet)"). Mirrors that checkout's internal/e2e/egresspolicy.go
// (EnsureEgressPolicy), without the testing.T dependency.
//
// This file imports Substrate's internal packages (internal/ateclient, internal/resources), so
// it cannot be built as a standalone Go module outside a Substrate checkout. To use it: drop
// this file into <substrate-checkout>/cmd/mainloop-egress-tool/main.go and build with
// `GOFLAGS=-mod=vendor go build -o mainloop-egress-tool ./cmd/mainloop-egress-tool` from the
// checkout root (module github.com/agent-substrate/substrate, pinned commit
// cdac9baef81dd319b46086d695266e6161e9e592 when this was written).
//
// Usage: mainloop-egress-tool --kubeconfig <path> --context <ctx> --atespace <as> --actor <name>
// --deny-all | --cidr <cidr> | --allow-all
//
// Fails closed: exactly one of --deny-all, --cidr, or --allow-all is required. An earlier version of this
// tool silently allowed all destinations whenever --cidr was omitted (see
// .tasknotes/gate5-review-and-recovery-plan-2026-09-22.md, "Make missing egress configuration
// fail closed"); --allow-all must now be passed explicitly to get that behavior.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// validateEgressInput is the fail-closed check, isolated as a pure function so it can be
// exercised without a cluster or a Substrate checkout.
func validateEgressInput(cidr string, denyAll, allowAll bool) error {
	selected := 0
	if cidr != "" {
		selected++
	}
	if denyAll {
		selected++
	}
	if allowAll {
		selected++
	}
	if selected != 1 {
		return fmt.Errorf("exactly one of --deny-all, --cidr <cidr>, or --allow-all is required")
	}
	return nil
}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "")
	context_ := flag.String("context", "", "")
	atespace := flag.String("atespace", "", "")
	actorName := flag.String("actor", "", "")
	cidr := flag.String("cidr", "", "CIDR to allow")
	denyAll := flag.Bool("deny-all", false, "explicitly deny all actor egress")
	allowAll := flag.Bool("allow-all", false, "explicitly allow all egress destinations")
	flag.Parse()

	if err := validateEgressInput(*cidr, *denyAll, *allowAll); err != nil {
		log.Fatalf("%v", err)
	}

	ctx := context.Background()
	cli, err := ateclient.NewClient(ctx, *kubeconfig, *context_, "", "", false)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	actorRef := resources.ActorRef{Atespace: *atespace, Name: *actorName}.ToObjectRef()

	var rules []*ateapipb.EgressRule
	if *allowAll {
		rules = []*ateapipb.EgressRule{{All: &emptypb.Empty{}}}
	} else if *cidr != "" {
		rules = []*ateapipb.EgressRule{{Cidrs: &ateapipb.CIDRRule{Cidrs: []string{*cidr}}}}
	}
	policy := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: *atespace, Name: "default"},
		Rules:    rules,
	}

	_, err = cli.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
		Actor:        actorRef,
		EgressPolicy: policy,
	})
	if status.Code(err) == codes.AlreadyExists {
		existing, gerr := cli.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: actorRef})
		if gerr != nil {
			log.Fatalf("get existing: %v", gerr)
		}
		policy.Metadata = existing.GetMetadata()
		if _, uerr := cli.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{
			Actor:        actorRef,
			EgressPolicy: policy,
		}); uerr != nil {
			log.Fatalf("update: %v", uerr)
		}
		fmt.Println("updated existing egress policy")
		return
	}
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	fmt.Println("created egress policy")
}

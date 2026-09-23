package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "")
	contextName := flag.String("context", "", "")
	atespace := flag.String("atespace", "", "")
	actor := flag.String("actor", "", "")
	hostname := flag.String("hostname", "", "")
	header := flag.String("header", "", "")
	prefix := flag.String("prefix", "", "")
	uri := flag.String("credential-uri", "", "")
	flag.Parse()
	ctx := context.Background()
	cli, err := ateclient.NewClient(ctx, *kubeconfig, *contextName, "", "", false)
	if err != nil {
		log.Fatal("ateapi client unavailable")
	}
	defer cli.Close()
	ref := resources.ActorRef{Atespace: *atespace, Name: *actor}.ToObjectRef()
	existing, err := cli.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: ref})
	if err != nil {
		log.Fatal("actor egress policy unavailable")
	}
	policy := &ateapipb.EgressPolicy{Metadata: existing.GetMetadata(), Rules: []*ateapipb.EgressRule{{
		Hostnames: &ateapipb.HostnameRule{Patterns: []string{*hostname}, Effects: &ateapipb.EgressRuleEffects{
			InjectStaticHeaders: []*ateapipb.CredentialHeaderInjection{{Header: *header, Prefix: *prefix, CredentialUri: *uri}},
		}},
	}}}
	if _, err := cli.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: policy}); err != nil {
		log.Fatal("injection policy update failed")
	}
	fmt.Println("egress_header_injection_policy=updated")
}

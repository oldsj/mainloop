package privileged

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siderolabs/go-api-signature/pkg/message"
	"github.com/siderolabs/go-api-signature/pkg/pgp"
	"github.com/siderolabs/go-api-signature/pkg/serviceaccount"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// fakeOmni stands for Omni: it records each call and answers with the request, after checking the
// call is signed by the executor's real key.
type fakeOmni struct {
	mu    sync.Mutex
	calls []omniCall
	real  *serviceaccount.ServiceAccount
	// reply, if set, answers a call; otherwise Omni echoes the request.
	reply func(method string, body []byte) []byte
}

type omniCall struct {
	method string
	md     metadata.MD
	body   []byte
	sigErr error
}

func (f *fakeOmni) handle(_ any, ss grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(ss)
	md, _ := metadata.FromIncomingContext(ss.Context())
	var body []byte
	if err := ss.RecvMsg(&body); err != nil {
		return err
	}
	msg := message.NewGRPC(md.Copy(), method)
	sigErr := msg.VerifySignature(f.real.Key)
	if sig, err := msg.Signature(); err == nil && sig.Identity != f.real.Name {
		sigErr = errors.New("identity " + sig.Identity)
	}
	f.mu.Lock()
	f.calls = append(f.calls, omniCall{method: method, md: md, body: body, sigErr: sigErr})
	f.mu.Unlock()
	if sigErr != nil {
		return grpcstatus.Error(codes.Unauthenticated, "bad signature")
	}
	if f.reply != nil {
		body = f.reply(method, body)
	}
	return ss.SendMsg(&body)
}

func (f *fakeOmni) seen() []omniCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// newProxyFixture starts a fake Omni, a proxy for one run in front of it, and returns a client
// connection to the proxy that signs as the run's binary would, with the run's key.
func newProxyFixture(t *testing.T, tool, cluster string, preflight bool) (*fakeOmni, *sideroProxy, *grpc.ClientConn) {
	t.Helper()
	key, err := pgp.GenerateKey("mainloop-executor", "", "mainloop-executor@serviceaccount.omni.example", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	real := &serviceaccount.ServiceAccount{Name: "mainloop-executor@serviceaccount.omni.example", Key: key}
	omni, upstream := startFakeOmni(t, real)
	p, err := newSideroProxy(real, upstream, tool, cluster, preflight, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	pln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.serve(pln))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	conn, err := grpc.NewClient(pln.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})),
		grpc.WithUnaryInterceptor(signAs(sideroRunIdentity, p.runKey)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return omni, p, conn
}

// startFakeOmni serves a fake Omni on loopback that checks calls are signed by real, and returns
// a connection to it.
func startFakeOmni(t *testing.T, real *serviceaccount.ServiceAccount) (*fakeOmni, *grpc.ClientConn) {
	t.Helper()
	omni := &fakeOmni{real: real}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(omni.handle))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return omni, conn
}

// cosiRequest encodes a COSI State request naming ns and typ, as fields 1 and 2.
func cosiRequest(ns, typ string) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendString(b, ns)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendString(b, typ)
}

// cosiResource wraps a metadata message (ns, typ) as a resource, at field of the request.
func cosiResource(field protowire.Number, ns, typ string) []byte {
	res := protowire.AppendTag(nil, 1, protowire.BytesType)
	res = protowire.AppendBytes(res, cosiRequest(ns, typ))
	b := protowire.AppendTag(nil, field, protowire.BytesType)
	return protowire.AppendBytes(b, res)
}

// signAs signs each call as Sidero's client interceptor does (go-api-signature's
// client/interceptor, which omnictl and talosctl use): message.Sign over the outgoing metadata.
func signAs(identity string, key *pgp.Key) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		}
		msg := message.NewGRPC(md.Copy(), method)
		if err := msg.Sign(identity, key); err != nil {
			return err
		}
		return invoker(metadata.NewOutgoingContext(ctx, msg.Metadata), method, req, reply, cc, opts...)
	}
}

func call(ctx context.Context, conn *grpc.ClientConn, method string, body []byte, kv ...string) error {
	if len(kv) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, kv...)
	}
	var out []byte
	return conn.Invoke(ctx, method, &body, &out, grpc.ForceCodec(rawCodec{}))
}

// Given a run's binary signing with the run key, when it calls an allowed method, then Omni gets
// the call signed by the executor's real key, with the routing headers and nothing else, and the
// body unchanged.
func TestSideroProxyResignsAllowedCalls(t *testing.T) {
	omni, _, conn := newProxyFixture(t, ToolTalosctl, "internal-01", false)
	ctx := context.Background()
	body := cosiRequest("network", "Routes.net.talos.dev")
	if err := call(ctx, conn, cosiState+"Get", body, "nodes", "192.0.2.11", "context", "internal-01", "x-extra", "dropped"); err != nil {
		t.Fatal(err)
	}
	if err := call(ctx, conn, "/machine.MachineService/Reboot", []byte{}, "nodes", "192.0.2.11", "context", "internal-01"); err != nil {
		t.Fatal(err)
	}
	calls := omni.seen()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	if c.sigErr != nil || c.method != cosiState+"Get" || string(c.body) != string(body) {
		t.Fatalf("call = %+v", c)
	}
	if !slices.Equal(c.md.Get("nodes"), []string{"192.0.2.11"}) || len(c.md.Get("x-extra")) > 0 ||
		strings.Contains(strings.Join(c.md.Get(message.SignatureHeaderKey), ""), sideroRunIdentity) {
		t.Fatalf("headers = %v", c.md)
	}
}

// Every refused call ends at the proxy: Omni never sees it.
func TestSideroProxyRefusals(t *testing.T) {
	ctx := context.Background()
	talos := []string{"nodes", "192.0.2.11", "context", "internal-01"}
	for name, tc := range map[string]struct {
		tool      string
		preflight bool
		method    string
		body      []byte
		md        []string
		code      codes.Code
	}{
		"kubeconfig":          {ToolOmnictl, false, "/management.ManagementService/Kubeconfig", nil, nil, codes.PermissionDenied},
		"talosconfig":         {ToolOmnictl, false, "/management.ManagementService/Talosconfig", nil, nil, codes.PermissionDenied},
		"service account":     {ToolOmnictl, false, "/management.ManagementService/CreateServiceAccount", nil, nil, codes.PermissionDenied},
		"support bundle":      {ToolOmnictl, false, "/management.ManagementService/GetSupportBundle", nil, nil, codes.PermissionDenied},
		"resource service":    {ToolOmnictl, false, "/omni.resources.ResourceService/Get", nil, nil, codes.PermissionDenied},
		"file read":           {ToolTalosctl, false, "/machine.MachineService/Read", nil, talos, codes.PermissionDenied},
		"client certificate":  {ToolTalosctl, false, "/machine.MachineService/GenerateClientConfiguration", nil, talos, codes.PermissionDenied},
		"etcd snapshot":       {ToolTalosctl, false, "/machine.MachineService/EtcdSnapshot", nil, talos, codes.PermissionDenied},
		"config apply":        {ToolTalosctl, false, "/machine.MachineService/ApplyConfiguration", nil, talos, codes.PermissionDenied},
		"cluster secrets":     {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "ClusterSecrets.omni.sidero.dev"), nil, codes.PermissionDenied},
		"list kubeconfigs":    {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "Kubeconfigs.omni.sidero.dev"), nil, codes.PermissionDenied},
		"watch join tokens":   {ToolOmnictl, false, cosiState + "Watch", cosiRequest("default", "JoinTokens.omni.sidero.dev"), nil, codes.PermissionDenied},
		"machine config":      {ToolTalosctl, false, cosiState + "Get", cosiRequest("config", "MachineConfigs.config.talos.dev"), talos, codes.PermissionDenied},
		"secrets namespace":   {ToolTalosctl, false, cosiState + "List", cosiRequest("secrets", "Anything.talos.dev"), talos, codes.PermissionDenied},
		"create a user":       {ToolOmnictl, false, cosiState + "Create", cosiResource(1, "default", "Users.omni.sidero.dev"), nil, codes.PermissionDenied},
		"update an identity":  {ToolOmnictl, false, cosiState + "Update", cosiResource(2, "default", "Identities.omni.sidero.dev"), nil, codes.PermissionDenied},
		"repeated type field": {ToolOmnictl, false, cosiState + "Get", append(cosiRequest("default", "Clusters.omni.sidero.dev"), cosiRequest("", "ClusterSecrets.omni.sidero.dev")[2:]...), nil, codes.PermissionDenied},
		"no type":             {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", ""), nil, codes.PermissionDenied},
		"write in preflight":  {ToolOmnictl, true, cosiState + "Destroy", cosiRequest("default", "ConfigPatches.omni.sidero.dev"), nil, codes.PermissionDenied},
		"reboot in preflight": {ToolTalosctl, true, "/machine.MachineService/Reboot", nil, talos, codes.PermissionDenied},
		"omnictl to talos":    {ToolOmnictl, false, "/machine.MachineService/Version", nil, talos, codes.PermissionDenied},
		"talosctl to omni":    {ToolTalosctl, false, cosiState + "Get", cosiRequest("default", "Clusters.omni.sidero.dev"), nil, codes.PermissionDenied},
		"another cluster":     {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"nodes", "n1", "context", "prod-02"}, codes.PermissionDenied},
		"two contexts":        {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"context", "internal-01", "context", "prod-02"}, codes.PermissionDenied},
		"cluster header":      {ToolTalosctl, false, "/machine.MachineService/Version", nil, append(slices.Clone(talos), "cluster", "prod-02"), codes.PermissionDenied},
		"authorization":       {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "Clusters.omni.sidero.dev"), []string{"authorization", "Bearer x"}, codes.PermissionDenied},
	} {
		t.Run(name, func(t *testing.T) {
			omni, _, conn := newProxyFixture(t, tc.tool, "internal-01", tc.preflight)
			err := call(ctx, conn, tc.method, tc.body, tc.md...)
			if grpcstatus.Code(err) != tc.code || !strings.Contains(err.Error(), "refused by the executor") {
				t.Fatalf("err = %v", err)
			}
			if calls := omni.seen(); len(calls) != 0 {
				t.Fatalf("Omni saw %+v", calls)
			}
		})
	}
}

// A call signed with any key but the run's, or not signed, is refused: a proxy that forwarded it
// would sign for whoever reached it.
func TestSideroProxyNeedsTheRunKey(t *testing.T) {
	omni, p, _ := newProxyFixture(t, ToolOmnictl, "", false)
	other, err := pgp.GenerateKey(sideroRunIdentity, "", sideroRunIdentity+"@executor.invalid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.serve(ln))
	for name, opts := range map[string][]grpc.DialOption{
		"another key": func() []grpc.DialOption {
			return []grpc.DialOption{grpc.WithUnaryInterceptor(signAs(sideroRunIdentity, other))}
		}(),
		"unsigned": nil,
	} {
		conn, err := grpc.NewClient(ln.Addr().String(), append(opts,
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})))...)
		if err != nil {
			t.Fatal(err)
		}
		err = call(context.Background(), conn, cosiState+"Get", cosiRequest("default", "Clusters.omni.sidero.dev"))
		_ = conn.Close()
		if grpcstatus.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if calls := omni.seen(); len(calls) != 0 {
		t.Fatalf("Omni saw %+v", calls)
	}
}

// The secret-type rule refuses Omni's and Talos's secret-bearing types and lets ordinary ones
// through.
func TestSecretTypes(t *testing.T) {
	for _, typ := range []string{
		"ClusterSecrets.omni.sidero.dev", "ClusterMachineConfigs.omni.sidero.dev", "Kubeconfigs.omni.sidero.dev",
		"TalosConfigs.omni.sidero.dev", "JoinTokens.omni.sidero.dev", "DefaultJoinTokens.omni.sidero.dev",
		"PublicKeys.omni.sidero.dev", "Identities.omni.sidero.dev", "Users.omni.sidero.dev", "AccessPolicies.omni.sidero.dev",
		"EtcdBackupS3Configs.omni.sidero.dev", "BackupDatas.omni.sidero.dev", "ClusterMachineEncryptionKeys.omni.sidero.dev",
		"ImportedClusterSecrets.omni.sidero.dev", "MachineJoinConfigs.omni.sidero.dev", "NodeUniqueTokens.omni.sidero.dev",
		"InfraMachineBMCConfigs.omni.sidero.dev", "SAMLAssertions.omni.sidero.dev", "ServiceAccountStatuses.omni.sidero.dev",
		"MachineConfigs.config.talos.dev", "OSRootSecrets.secrets.talos.dev", "KubeSpanIdentities.kubespan.talos.dev",
		"machineconfig", "clustersecrets", "MachineConfigDiffs.omni.sidero.dev",
	} {
		if !secretType(typ) {
			t.Errorf("%s is not refused", typ)
		}
	}
	for _, typ := range []string{
		"Clusters.omni.sidero.dev", "Machines.omni.sidero.dev", "ClusterStatuses.omni.sidero.dev", "ConfigPatches.omni.sidero.dev",
		"MachineSets.omni.sidero.dev", "RedactedClusterMachineConfigs.omni.sidero.dev", "Members.cluster.talos.dev",
		"Routes.net.talos.dev", "Services.v1alpha1.talos.dev", "KubernetesUsages.omni.sidero.dev", "clusters", "members",
	} {
		if secretType(typ) {
			t.Errorf("%s is refused", typ)
		}
	}
}

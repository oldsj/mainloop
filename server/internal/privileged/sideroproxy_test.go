package privileged

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
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

// fakeOmni stands for Omni: it records each call and answers it, after checking the call is
// signed by the executor's real key.
type fakeOmni struct {
	mu    sync.Mutex
	calls []omniCall
	real  *serviceaccount.ServiceAccount
	// reply, if set, answers a call with any number of messages; otherwise Omni echoes the
	// request.
	reply func(method string, body []byte) ([][]byte, error)
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
	msgs := [][]byte{body}
	if f.reply != nil {
		var err error
		if msgs, err = f.reply(method, body); err != nil {
			return err
		}
	}
	for _, m := range msgs {
		if err := ss.SendMsg(&m); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeOmni) seen() []omniCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// forwarded says whether Omni was sent this exact request: the proxy's own lookups are other
// requests.
func (f *fakeOmni) forwarded(method string, body []byte) bool {
	return slices.ContainsFunc(f.seen(), func(c omniCall) bool { return c.method == method && bytes.Equal(c.body, body) })
}

// omniWorld is what the fake Omni holds: resources, the machines of two clusters, and a resource
// definition for every type (sensitive, sensitive only in its YAML, or missing, as listed).
type omniWorld struct {
	resources     []cosiResource
	sensitive     []string
	yamlSensitive []string
	noDefinition  []string
}

// testWorld has clusters internal-01 (machine m-1, node-a, 192.0.2.11, on Talos 1.14.2 with
// schematic ab…; and m-3, node-b, 192.0.2.12, whose schematic Omni cannot tell) and prod-02 (m-2,
// node-z, 192.0.2.99), a machine set and a cluster resource of each, and a machine class of
// neither.
func testWorld() *omniWorld {
	machine := func(id, cluster, name, ip string) cosiResource {
		spec := protowire.AppendTag(nil, 3, protowire.BytesType)
		spec = protowire.AppendString(spec, name)
		spec = protowire.AppendTag(spec, 8, protowire.BytesType)
		spec = protowire.AppendString(spec, ip)
		return cosiResource{ns: "default", typ: omniClusterMachineIDs, id: id, labels: map[string]string{omniClusterLabel: cluster}, protoSpec: spec}
	}
	in := func(cluster string) map[string]string { return map[string]string{omniClusterLabel: cluster} }
	// MachineStatusSpec: talos_version (1), schematic (14) with invalid (2) and full_id (9), and
	// kernel_cmdline (22), which the proxy never passes on.
	status := func(id, cluster, version, schematic string, invalid bool) cosiResource {
		sch := protowire.AppendTag(nil, 9, protowire.BytesType)
		sch = protowire.AppendString(sch, schematic)
		if invalid {
			sch = protowire.AppendTag(sch, 2, protowire.VarintType)
			sch = protowire.AppendVarint(sch, 1)
		}
		spec := protowire.AppendTag(nil, 1, protowire.BytesType)
		spec = protowire.AppendString(spec, version)
		spec = protowire.AppendTag(spec, 14, protowire.BytesType)
		spec = protowire.AppendBytes(spec, sch)
		spec = protowire.AppendTag(spec, 22, protowire.BytesType)
		spec = protowire.AppendString(spec, "siderolink.api=https://omni.example.com?jointoken=fictional")
		return cosiResource{ns: "default", typ: omniMachineStatuses, id: id, labels: in(cluster), protoSpec: spec}
	}
	return &omniWorld{
		resources: []cosiResource{
			machine("m-1", "internal-01", "node-a", "192.0.2.11"),
			machine("m-2", "prod-02", "node-z", "192.0.2.99"),
			machine("m-3", "internal-01", "node-b", "192.0.2.12"),
			status("m-1", "internal-01", "1.14.2", strings.Repeat("ab", 32), false),
			status("m-2", "prod-02", "1.14.2", strings.Repeat("ab", 32), false),
			status("m-3", "internal-01", "1.14.2", "", true),
			{ns: "default", typ: "Clusters.omni.sidero.dev", id: "internal-01", labels: map[string]string{}},
			{ns: "default", typ: "Clusters.omni.sidero.dev", id: "prod-02", labels: map[string]string{}},
			{ns: "default", typ: "MachineSets.omni.sidero.dev", id: "internal-01-workers", labels: in("internal-01")},
			{ns: "default", typ: "MachineSets.omni.sidero.dev", id: "prod-02-workers", labels: in("prod-02")},
			{ns: "default", typ: "MachineClasses.omni.sidero.dev", id: "big", labels: map[string]string{}},
		},
		sensitive:     []string{"AddressStatuses.net.talos.dev", "OngoingTasks.omni.sidero.dev"},
		yamlSensitive: []string{"HostnameStatuses.net.talos.dev"},
		noDefinition:  []string{"ProbeStatuses.net.talos.dev"},
	}
}

func (w *omniWorld) reply(method string, body []byte) ([][]byte, error) {
	call, ok := strings.CutPrefix(method, cosiState)
	if !ok {
		return [][]byte{body}, nil
	}
	ns, _ := protoField(body, 1)
	typ, _ := protoField(body, 2)
	id, _ := protoField(body, 3)
	wrap := func(field protowire.Number, r cosiResource) []byte {
		b := protowire.AppendTag(nil, field, protowire.BytesType)
		return protowire.AppendBytes(b, encodeResource(r))
	}
	switch call {
	case "Get":
		if string(typ) == cosiResourceDefinitions {
			return w.definition(string(id))
		}
		for _, r := range w.resources {
			if r.ns == string(ns) && r.typ == string(typ) && r.id == string(id) {
				return [][]byte{wrap(1, r)}, nil
			}
		}
		if strings.HasSuffix(string(typ), ".omni.sidero.dev") {
			return nil, grpcstatus.Error(codes.NotFound, "not found")
		}
		return [][]byte{body}, nil
	case "List", "Watch":
		var out [][]byte
		var events []byte
		for _, r := range w.resources {
			if r.typ != string(typ) {
				continue
			}
			if call == "List" {
				out = append(out, wrap(1, r))
				continue
			}
			ev := protowire.AppendTag(wrap(1, r), 2, protowire.VarintType)
			ev = protowire.AppendVarint(ev, 1)
			events = protowire.AppendTag(events, 1, protowire.BytesType)
			events = protowire.AppendBytes(events, ev)
		}
		if call == "Watch" {
			out = append(out, events)
		}
		return out, nil
	}
	return [][]byte{body}, nil
}

func (w *omniWorld) definition(id string) ([][]byte, error) {
	has := func(list []string) bool {
		return slices.ContainsFunc(list, func(t string) bool { return strings.ToLower(t) == id })
	}
	if has(w.noDefinition) {
		return nil, grpcstatus.Error(codes.NotFound, "no such type")
	}
	r := cosiResource{ns: metaNamespace, typ: cosiResourceDefinitions, id: id, labels: map[string]string{}}
	if has(w.yamlSensitive) {
		r.yamlSpec = []byte("type: " + id + "\nsensitivity: sensitive\n")
	} else {
		r.protoSpec = protowire.AppendTag(nil, 1, protowire.BytesType)
		r.protoSpec = protowire.AppendString(r.protoSpec, id)
		if has(w.sensitive) {
			r.protoSpec = protowire.AppendTag(r.protoSpec, rdSensitivityField, protowire.VarintType)
			r.protoSpec = protowire.AppendVarint(r.protoSpec, rdSensitive)
		}
	}
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	return [][]byte{protowire.AppendBytes(b, encodeResource(r))}, nil
}

// encodeResource encodes a cosi.resource.Resource.
func encodeResource(r cosiResource) []byte {
	str := func(b []byte, n protowire.Number, v string) []byte {
		b = protowire.AppendTag(b, n, protowire.BytesType)
		return protowire.AppendString(b, v)
	}
	md := str(str(str(nil, 1, r.ns), 2, r.typ), 3, r.id)
	keys := make([]string, 0, len(r.labels))
	for k := range r.labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		md = protowire.AppendTag(md, 10, protowire.BytesType)
		md = protowire.AppendBytes(md, str(str(nil, 1, k), 2, r.labels[k]))
	}
	var spec []byte
	if r.protoSpec != nil {
		spec = protowire.AppendTag(spec, 1, protowire.BytesType)
		spec = protowire.AppendBytes(spec, r.protoSpec)
	}
	if r.yamlSpec != nil {
		spec = protowire.AppendTag(spec, 2, protowire.BytesType)
		spec = protowire.AppendBytes(spec, r.yamlSpec)
	}
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, md)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendBytes(b, spec)
}

// newProxyFixture starts a fake Omni holding testWorld, a proxy for one run in front of it (on
// cluster internal-01), and returns a client connection to the proxy that signs as the run's
// binary would, with the run's key.
func newProxyFixture(t *testing.T, tool, cluster string, preflight bool) (*fakeOmni, *sideroProxy, *grpc.ClientConn) {
	t.Helper()
	key, err := pgp.GenerateKey("mainloop-executor", "", "mainloop-executor@serviceaccount.omni.example", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	real := &serviceaccount.ServiceAccount{Name: "mainloop-executor@serviceaccount.omni.example", Key: key}
	omni, upstream := startFakeOmni(t, real)
	omni.reply = testWorld().reply
	p, err := newSideroProxy(real, upstream, tool, cluster, []string{"internal-01"}, preflight, time.Hour)
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
		grpc.WithUnaryInterceptor(signAs(sideroRunIdentity, p.runKey)),
		grpc.WithStreamInterceptor(signStreamAs(sideroRunIdentity, p.runKey)))
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
func cosiRequest(ns, typ string) []byte { return cosiList(ns, typ) }

// cosiWrite encodes a Create (field 1) or Update (field 2) of a resource.
func cosiWrite(field protowire.Number, r cosiResource) []byte {
	b := protowire.AppendTag(nil, field, protowire.BytesType)
	return protowire.AppendBytes(b, encodeResource(r))
}

// signAs signs each call as Sidero's client interceptor does (go-api-signature's
// client/interceptor, which omnictl and talosctl use): message.Sign over the outgoing metadata.
func signAs(identity string, key *pgp.Key) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, err := signed(ctx, method, identity, key)
		if err != nil {
			return err
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func signStreamAs(identity string, key *pgp.Key) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx, err := signed(ctx, method, identity, key)
		if err != nil {
			return nil, err
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func signed(ctx context.Context, method, identity string, key *pgp.Key) (context.Context, error) {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	}
	msg := message.NewGRPC(md.Copy(), method)
	if err := msg.Sign(identity, key); err != nil {
		return nil, err
	}
	return metadata.NewOutgoingContext(ctx, msg.Metadata), nil
}

func call(ctx context.Context, conn *grpc.ClientConn, method string, body []byte, kv ...string) error {
	_, err := callAll(ctx, conn, method, body, kv...)
	return err
}

// callAll makes a call and returns every answer.
func callAll(ctx context.Context, conn *grpc.ClientConn, method string, body []byte, kv ...string) ([][]byte, error) {
	if len(kv) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, kv...)
	}
	cs, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, method, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return nil, err
	}
	if err := cs.SendMsg(&body); err != nil {
		return nil, err
	}
	if err := cs.CloseSend(); err != nil {
		return nil, err
	}
	var out [][]byte
	for {
		var m []byte
		if err := cs.RecvMsg(&m); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return out, err
		}
		out = append(out, m)
	}
}

// ids are the IDs of the resources in a List's or Watch's answers.
func ids(t *testing.T, msgs [][]byte, watch bool) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		var resources [][]byte
		if watch {
			if err := protoEach(m, 1, func(ev []byte) error {
				r, err := protoField(ev, 1)
				resources = append(resources, r)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		} else {
			r, err := protoField(m, 1)
			if err != nil {
				t.Fatal(err)
			}
			resources = append(resources, r)
		}
		for _, b := range resources {
			r, err := parseCosiResource(b)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, r.id)
		}
	}
	return out
}

// Given a run's binary signing with the run key, when it calls an allowed method, then Omni gets
// the call signed by the executor's real key, with the routing headers and nothing else, and the
// body unchanged.
func TestSideroProxyResignsAllowedCalls(t *testing.T) {
	omni, _, conn := newProxyFixture(t, ToolTalosctl, "internal-01", false)
	ctx := context.Background()
	body := cosiRequest("network", "RouteStatuses.net.talos.dev")
	if err := call(ctx, conn, cosiState+"Get", body, "nodes", "192.0.2.11", "context", "internal-01", "x-extra", "dropped"); err != nil {
		t.Fatal(err)
	}
	if err := call(ctx, conn, "/machine.MachineService/Reboot", []byte{}, "nodes", "192.0.2.11", "context", "internal-01"); err != nil {
		t.Fatal(err)
	}
	if !omni.forwarded(cosiState+"Get", body) || !omni.forwarded("/machine.MachineService/Reboot", []byte{}) {
		t.Fatalf("calls = %+v", omni.seen())
	}
	for _, c := range omni.seen() {
		if c.sigErr != nil || len(c.md.Get("x-extra")) > 0 ||
			strings.Contains(strings.Join(c.md.Get(message.SignatureHeaderKey), ""), sideroRunIdentity) {
			t.Fatalf("call = %+v", c)
		}
		if bytes.Equal(c.body, body) && !slices.Equal(c.md.Get("nodes"), []string{"192.0.2.11"}) {
			t.Fatalf("headers = %v", c.md)
		}
	}
}

// Every refused call ends at the proxy: Omni never gets it (the proxy may have asked Omni
// something itself to decide).
func TestSideroProxyRefusals(t *testing.T) {
	ctx := context.Background()
	talos := []string{"nodes", "192.0.2.11", "context", "internal-01"}
	other := func(cluster string) map[string]string { return map[string]string{omniClusterLabel: cluster} }
	for name, tc := range map[string]struct {
		tool      string
		preflight bool
		method    string
		body      []byte
		md        []string
	}{
		"kubeconfig":          {ToolOmnictl, false, "/management.ManagementService/Kubeconfig", nil, nil},
		"talosconfig":         {ToolOmnictl, false, "/management.ManagementService/Talosconfig", nil, nil},
		"service account":     {ToolOmnictl, false, "/management.ManagementService/CreateServiceAccount", nil, nil},
		"support bundle":      {ToolOmnictl, false, "/management.ManagementService/GetSupportBundle", nil, nil},
		"resource service":    {ToolOmnictl, false, "/omni.resources.ResourceService/Get", nil, nil},
		"file read":           {ToolTalosctl, false, "/machine.MachineService/Read", nil, talos},
		"client certificate":  {ToolTalosctl, false, "/machine.MachineService/GenerateClientConfiguration", nil, talos},
		"etcd snapshot":       {ToolTalosctl, false, "/machine.MachineService/EtcdSnapshot", nil, talos},
		"config apply":        {ToolTalosctl, false, "/machine.MachineService/ApplyConfiguration", nil, talos},
		"cluster secrets":     {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "ClusterSecrets.omni.sidero.dev"), nil},
		"list kubeconfigs":    {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "Kubeconfigs.omni.sidero.dev"), nil},
		"watch join tokens":   {ToolOmnictl, false, cosiState + "Watch", cosiRequest("default", "JoinTokens.omni.sidero.dev"), nil},
		"machine config":      {ToolTalosctl, false, cosiState + "Get", cosiRequest("config", "MachineConfigs.config.talos.dev"), talos},
		"secrets namespace":   {ToolTalosctl, false, cosiState + "List", cosiRequest("secrets", "Members.cluster.talos.dev"), talos},
		"create a user":       {ToolOmnictl, false, cosiState + "Create", cosiWrite(1, cosiResource{ns: "default", typ: "Users.omni.sidero.dev", id: "x"}), nil},
		"update an identity":  {ToolOmnictl, false, cosiState + "Update", cosiWrite(2, cosiResource{ns: "default", typ: "Identities.omni.sidero.dev", id: "x"}), nil},
		"repeated type field": {ToolOmnictl, false, cosiState + "Get", append(cosiRequest("default", "Clusters.omni.sidero.dev"), cosiRequest("", "ClusterSecrets.omni.sidero.dev")[2:]...), nil},
		"no type":             {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", ""), nil},
		"write in preflight":  {ToolOmnictl, true, cosiState + "Destroy", cosiGet("default", "MachineSets.omni.sidero.dev", "internal-01-workers"), nil},
		"reboot in preflight": {ToolTalosctl, true, "/machine.MachineService/Reboot", nil, talos},
		"omnictl to talos":    {ToolOmnictl, false, "/machine.MachineService/Version", nil, talos},
		"talosctl to omni":    {ToolTalosctl, false, cosiState + "Get", cosiRequest("default", "Clusters.omni.sidero.dev"), nil},
		"another cluster":     {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"nodes", "n1", "context", "prod-02"}},
		"two contexts":        {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"context", "internal-01", "context", "prod-02"}},
		"cluster header":      {ToolTalosctl, false, "/machine.MachineService/Version", nil, append(slices.Clone(talos), "cluster", "prod-02")},
		"authorization":       {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "Clusters.omni.sidero.dev"), []string{"authorization", "Bearer x"}},

		// The types the review named, which hold secrets and no name pattern caught: not on the
		// allowlist.
		"omni config patches":       {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "ConfigPatches.omni.sidero.dev"), nil},
		"omni siderolink config":    {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "SiderolinkConfigs.omni.sidero.dev"), nil},
		"link specs":                {ToolTalosctl, false, cosiState + "List", cosiRequest("network", "LinkSpecs.net.talos.dev"), talos},
		"registries":                {ToolTalosctl, false, cosiState + "Get", cosiRequest("cri", "RegistryConfigs.cri.talos.dev"), talos},
		"manifests":                 {ToolTalosctl, false, cosiState + "List", cosiRequest("controlplane", "Manifests.kubernetes.talos.dev"), talos},
		"kubespan config":           {ToolTalosctl, false, cosiState + "Get", cosiRequest("config", "KubeSpanConfigs.kubespan.talos.dev"), talos},
		"discovery config":          {ToolTalosctl, false, cosiState + "Get", cosiRequest("config", "DiscoveryConfigs.cluster.talos.dev"), talos},
		"volume configs":            {ToolTalosctl, false, cosiState + "List", cosiRequest("runtime", "VolumeConfigs.block.talos.dev"), talos},
		"extension service configs": {ToolTalosctl, false, cosiState + "List", cosiRequest("runtime", "ExtensionServiceConfigs.runtime.talos.dev"), talos},
		"talos siderolink config":   {ToolTalosctl, false, cosiState + "Get", cosiRequest("config", "SiderolinkConfigs.siderolink.talos.dev"), talos},
		"type in other case":        {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "clusters.omni.sidero.dev"), nil},
		// What the server's resource definition says.
		"sensitive definition":      {ToolTalosctl, false, cosiState + "List", cosiRequest("network", "AddressStatuses.net.talos.dev"), talos},
		"sensitive in omni":         {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "OngoingTasks.omni.sidero.dev"), nil},
		"sensitive in yaml":         {ToolTalosctl, false, cosiState + "Get", cosiRequest("network", "HostnameStatuses.net.talos.dev"), talos},
		"no definition":             {ToolTalosctl, false, cosiState + "List", cosiRequest("network", "ProbeStatuses.net.talos.dev"), talos},
		// talosctl reaches only its cluster's machines, and writes no resource.
		"node of another cluster": {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"nodes", "192.0.2.99", "context", "internal-01"}},
		"unknown node":            {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"node", "198.51.100.7", "context", "internal-01"}},
		"one node of two":         {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"nodes", "192.0.2.11,node-z", "context", "internal-01"}},
		"no node":                 {ToolTalosctl, false, "/machine.MachineService/Version", nil, []string{"context", "internal-01"}},
		"talosctl write":          {ToolTalosctl, false, cosiState + "Create", cosiWrite(1, cosiResource{ns: "cluster", typ: "Members.cluster.talos.dev", id: "x"}), talos},
		"meta write":              {ToolOmnictl, false, cosiState + "Destroy", cosiGet("meta", cosiResourceDefinitions, "clusters.omni.sidero.dev"), nil},
		"upgrade to any image":    {ToolTalosctl, false, methodUpgrade, cosiRequest("docker.io/someone/installer:v1", ""), talos},
		// omnictl changes only resources of its clusters.
		"create in another cluster":   {ToolOmnictl, false, cosiState + "Create", cosiWrite(1, cosiResource{ns: "default", typ: "MachineSets.omni.sidero.dev", id: "x", labels: other("prod-02")}), nil},
		"create in no cluster":        {ToolOmnictl, false, cosiState + "Create", cosiWrite(1, cosiResource{ns: "default", typ: "MachineClasses.omni.sidero.dev", id: "x"}), nil},
		"create another cluster":      {ToolOmnictl, false, cosiState + "Create", cosiWrite(1, cosiResource{ns: "default", typ: "Clusters.omni.sidero.dev", id: "prod-03"}), nil},
		"relabel into ours":           {ToolOmnictl, false, cosiState + "Update", cosiWrite(2, cosiResource{ns: "default", typ: "MachineSets.omni.sidero.dev", id: "prod-02-workers", labels: other("internal-01")}), nil},
		"destroy another cluster's":   {ToolOmnictl, false, cosiState + "Destroy", cosiGet("default", "MachineSets.omni.sidero.dev", "prod-02-workers"), nil},
		"teardown another cluster":    {ToolOmnictl, false, cosiState + "Teardown", cosiGet("default", "Clusters.omni.sidero.dev", "prod-02"), nil},
		"destroy of no cluster":       {ToolOmnictl, false, cosiState + "Destroy", cosiGet("default", "MachineClasses.omni.sidero.dev", "big"), nil},
		"destroy what is not there":   {ToolOmnictl, false, cosiState + "Destroy", cosiGet("default", "MachineSets.omni.sidero.dev", "gone"), nil},
		// The kernel log prints the kernel command line, which may hold the SideroLink join token;
		// so do these types.
		"machine logs":           {ToolOmnictl, false, "/management.ManagementService/MachineLogs", cosiRequest("m-1", ""), nil},
		"dmesg":                  {ToolTalosctl, false, "/machine.MachineService/Dmesg", nil, talos},
		"machine statuses":       {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "MachineStatuses.omni.sidero.dev"), nil},
		"kernel args":            {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "KernelArgs.omni.sidero.dev"), nil},
		"kernel args statuses":   {ToolOmnictl, false, cosiState + "Get", cosiRequest("default", "KernelArgsStatuses.omni.sidero.dev"), nil},
		"pending config diff":    {ToolOmnictl, false, cosiState + "List", cosiRequest("default", "MachinePendingUpdates.omni.sidero.dev"), nil},
		// Talos 1.13's upgrade: pull, then install, each with the image.
		"pull any image":         {ToolTalosctl, false, methodImagePull, imagePull("docker.io/someone/installer:v1"), talos},
		"pull no image":          {ToolTalosctl, false, methodImagePull, []byte{}, talos},
		"install any image":      {ToolTalosctl, false, methodLifecycleUpgrade, lifecycleUpgrade("docker.io/someone/installer:v1"), talos},
		"install with no source": {ToolTalosctl, false, methodLifecycleUpgrade, []byte{}, talos},
		"pull in preflight":      {ToolTalosctl, true, methodImagePull, imagePull("ghcr.io/siderolabs/installer:v1.14.5"), talos},
		// An upgrade holds to the machine's own schematic (ab…) and to Talos 1.14.2 or later.
		"install another schematic":       {ToolTalosctl, false, methodLifecycleUpgrade, lifecycleUpgrade(factoryImage("cd", "v1.14.5")), talos},
		"pull a downgrade":                {ToolTalosctl, false, methodImagePull, imagePull(factoryImage("ab", "v1.14.1")), talos},
		"legacy downgrade":                {ToolTalosctl, false, methodUpgrade, cosiRequest(factoryImage("ab", "v1.13.9"), ""), talos},
		"the vanilla installer":           {ToolTalosctl, false, methodUpgrade, cosiRequest("ghcr.io/siderolabs/installer:v1.14.5", ""), talos},
		"an image with no version":        {ToolTalosctl, false, methodImagePull, imagePull("factory.talos.dev/metal-installer/" + strings.Repeat("ab", 32)), talos},
		"a version that is not semver":    {ToolTalosctl, false, methodImagePull, imagePull(factoryImage("ab", "latest")), talos},
		"legacy upgrade with no image":    {ToolTalosctl, false, methodUpgrade, []byte{}, talos},
		"a machine with no schematic":     {ToolTalosctl, false, methodImagePull, imagePull(factoryImage("ab", "v1.14.5")), []string{"nodes", "192.0.2.12", "context", "internal-01"}},
		"one good node and one not":       {ToolTalosctl, false, methodImagePull, imagePull(factoryImage("ab", "v1.14.5")), []string{"nodes", "192.0.2.11,node-b", "context", "internal-01"}},
	} {
		t.Run(name, func(t *testing.T) {
			omni, _, conn := newProxyFixture(t, tc.tool, "internal-01", tc.preflight)
			err := call(ctx, conn, tc.method, tc.body, tc.md...)
			if grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "refused by the executor") {
				t.Fatalf("err = %v", err)
			}
			if omni.forwarded(tc.method, tc.body) {
				t.Fatalf("Omni got the request: %+v", omni.seen())
			}
		})
	}
}

// Given Omni manages internal-01 and prod-02, when omnictl reads and writes through a proxy held
// to internal-01, then it changes internal-01's resources, and sees none of prod-02's.
func TestSideroProxyHoldsOmnictlToItsClusters(t *testing.T) {
	ctx := context.Background()
	omni, _, conn := newProxyFixture(t, ToolOmnictl, "", false)
	for _, w := range []struct {
		method string
		body   []byte
	}{
		{cosiState + "Create", cosiWrite(1, cosiResource{ns: "default", typ: "MachineSets.omni.sidero.dev", id: "internal-01-gpu", labels: map[string]string{omniClusterLabel: "internal-01"}})},
		{cosiState + "Update", cosiWrite(2, cosiResource{ns: "default", typ: "MachineSets.omni.sidero.dev", id: "internal-01-workers", labels: map[string]string{omniClusterLabel: "internal-01"}})},
		{cosiState + "Destroy", cosiGet("default", "MachineSets.omni.sidero.dev", "internal-01-workers")},
		{cosiState + "Teardown", cosiGet("default", "Clusters.omni.sidero.dev", "internal-01")},
	} {
		if _, err := callAll(ctx, conn, w.method, w.body); err != nil || !omni.forwarded(w.method, w.body) {
			t.Fatalf("%s: %v", w.method, err)
		}
	}
	got, err := callAll(ctx, conn, cosiState+"List", cosiRequest("default", "MachineSets.omni.sidero.dev"))
	if err != nil || !slices.Equal(ids(t, got, false), []string{"internal-01-workers"}) {
		t.Fatalf("List = %v, %v", got, err)
	}
	got, err = callAll(ctx, conn, cosiState+"List", cosiRequest("default", "Clusters.omni.sidero.dev"))
	if err != nil || !slices.Equal(ids(t, got, false), []string{"internal-01"}) {
		t.Fatalf("List = %v, %v", got, err)
	}
	got, err = callAll(ctx, conn, cosiState+"List", cosiRequest("default", "MachineClasses.omni.sidero.dev"))
	if err != nil || !slices.Equal(ids(t, got, false), []string{"big"}) {
		t.Fatalf("a resource of no cluster: List = %v, %v", got, err)
	}
	got, err = callAll(ctx, conn, cosiState+"Watch", cosiRequest("default", "MachineSets.omni.sidero.dev"))
	if err != nil || !slices.Equal(ids(t, got, true), []string{"internal-01-workers"}) {
		t.Fatalf("Watch = %v, %v", got, err)
	}
	if _, err := callAll(ctx, conn, cosiState+"Get", cosiGet("default", "Clusters.omni.sidero.dev", "internal-01")); err != nil {
		t.Fatal(err)
	}
	_, err = callAll(ctx, conn, cosiState+"Get", cosiGet("default", "Clusters.omni.sidero.dev", "prod-02"))
	if grpcstatus.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "a cluster the executor does not act on") {
		t.Fatalf("Get prod-02: %v", err)
	}
}

// talosctl reaches a node of its cluster by address, name or machine ID, and upgrades to an image
// from Sidero's registries.
func TestSideroProxyTalosNodes(t *testing.T) {
	ctx := context.Background()
	omni, _, conn := newProxyFixture(t, ToolTalosctl, "internal-01", false)
	for _, node := range []string{"192.0.2.11", "node-a", "NODE-A", "m-1", "::ffff:192.0.2.11"} {
		if err := call(ctx, conn, "/machine.MachineService/Version", []byte{}, "node", node, "context", "internal-01"); err != nil {
			t.Errorf("%s: %v", node, err)
		}
	}
	image := "factory.talos.dev/metal-installer/" + strings.Repeat("ab", 32) + ":v1.14.5"
	if err := call(ctx, conn, methodUpgrade, cosiRequest(image, ""), "node", "node-a", "context", "internal-01"); err != nil || !omni.forwarded(methodUpgrade, cosiRequest(image, "")) {
		t.Fatalf("upgrade: %v", err)
	}
	lists := 0
	for _, c := range omni.seen() {
		if c.method == cosiState+"List" {
			lists++
		}
	}
	if lists != 1 {
		t.Errorf("the machines were listed %d times, want once per run", lists)
	}
	// Talos 1.13 and later: talosctl pulls the image, installs it, then reboots. The same version
	// (a reinstall), a pre-release of a later one, and a secure-boot installer of the same
	// schematic are upgrades too.
	for method, body := range map[string][]byte{
		methodImagePull:        imagePull(image),
		methodLifecycleUpgrade: lifecycleUpgrade(image),
		methodUpgrade:          cosiRequest(factoryImage("ab", "v1.14.2"), ""),
		methodImagePull + "#2": imagePull(factoryImage("ab", "v1.15.0-alpha.1")),
		methodImagePull + "#3": imagePull("factory.talos.dev/metal-installer-secureboot/" + strings.Repeat("ab", 32) + ":v1.14.5"),
	} {
		method, _, _ = strings.Cut(method, "#")
		if err := call(ctx, conn, method, body, "node", "node-a", "context", "internal-01"); err != nil || !omni.forwarded(method, body) {
			t.Fatalf("%s: %v", method, err)
		}
	}
}

// factoryImage is an Image Factory installer for the schematic of 32 repeats of hex.
func factoryImage(hex, version string) string {
	return "factory.talos.dev/metal-installer/" + strings.Repeat(hex, 32) + ":" + version
}

// imagePull encodes an ImageServicePullRequest: containerd (1), image_ref (3).
func imagePull(ref string) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte{})
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	return protowire.AppendString(b, ref)
}

// lifecycleUpgrade encodes a LifecycleServiceUpgradeRequest: source (2) with image_name (1).
func lifecycleUpgrade(image string) []byte {
	src := protowire.AppendTag(nil, 1, protowire.BytesType)
	src = protowire.AppendString(src, image)
	b := protowire.AppendTag(nil, 2, protowire.BytesType)
	return protowire.AppendBytes(b, src)
}

// The parser's allowlist names types as the tools do: full name, plural or singular.
func TestSideroTypeNames(t *testing.T) {
	for _, tc := range []struct{ tool, word, want string }{
		{ToolOmnictl, "clusters", "Clusters.omni.sidero.dev"},
		{ToolOmnictl, "cluster", "Clusters.omni.sidero.dev"},
		{ToolOmnictl, "MachineSets.omni.sidero.dev", "MachineSets.omni.sidero.dev"},
		{ToolOmnictl, "machineclass", "MachineClasses.omni.sidero.dev"},
		{ToolTalosctl, "members", "Members.cluster.talos.dev"},
		{ToolTalosctl, "addressstatus", "AddressStatuses.net.talos.dev"},
		{ToolTalosctl, "service", "Services.v1alpha1.talos.dev"},
	} {
		if got, ok := sideroTypeFor(tc.tool, tc.word); !ok || got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.tool, tc.word, got, tc.want)
		}
	}
	for _, typ := range append(slices.Clone(omniReadable), talosReadable...) {
		if secretType(typ) {
			t.Errorf("%s is on the allowlist and matches the secret pattern", typ)
		}
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

// The secret-type rule, a second check behind the allowlist, refuses Omni's and Talos's
// secret-bearing types by name and lets ordinary ones through (ConfigPatches among them: the
// allowlist refuses it).
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

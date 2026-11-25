package privileged

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/go-api-signature/pkg/message"
	"github.com/siderolabs/go-api-signature/pkg/pgp"
	"github.com/siderolabs/go-api-signature/pkg/serviceaccount"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip" // omnictl and talosctl compress their requests
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// sideroProxy is the executor's gRPC proxy for one omnictl or talosctl run, on loopback. Omni's
// request signature covers the method, a few routing headers and a timestamp, not the request
// body, so a signature that left the executor could be replayed with any body for minutes. The
// binary therefore never holds the executor's Omni key: it signs with a key made for the run, and
// the proxy checks that signature, applies its rules, and signs again with the real key before
// forwarding to Omni. Its rules hold whatever the command asked:
//   - only the methods the allowed commands use, by name; in preflight, only reads;
//   - only the resource types the executor reads (sideroReadable), never one whose resource
//     definition the server marks sensitive, nor one secretType matches, nor a secrets namespace;
//   - a talosctl run reaches only its target cluster's nodes, as Omni lists its machines, and
//     writes no resource; an omnictl run reaches only Omni, writes only resources of the clusters
//     the executor acts on, and sees none of another cluster's;
//   - machine logs of those clusters' machines only, and upgrades to Sidero's images only;
//   - one request message per call, and no authorization header.
type sideroProxy struct {
	tool    string
	cluster string
	// clusters are the clusters the run may act on: talosctl's target, or omnictl's list.
	clusters  []string
	preflight bool
	real      *serviceaccount.ServiceAccount
	upstream  *grpc.ClientConn
	// runName and runKey are the run's identity and key; runKeyB64 is them as a service-account
	// key, for the binary's environment.
	runName   string
	runKey    *pgp.Key
	runKeyB64 string
	tls       *tls.Config
	caPEM     []byte

	mu sync.Mutex
	// sensitivity caches resource definitions by node and type; clusterMachines, the clusters'
	// machines, once read.
	sensitivity     map[string]bool
	clusterMachines *clusterMachines
}

const sideroRunIdentity = "mainloop-executor-run"

// newSideroProxy makes the proxy for one run. upstream is the connection to Omni; the proxy does
// not close it. clusters are the clusters omnictl may act on; talosctl acts on cluster only.
func newSideroProxy(real *serviceaccount.ServiceAccount, upstream *grpc.ClientConn, tool, cluster string, clusters []string, preflight bool, lifetime time.Duration) (*sideroProxy, error) {
	key, err := pgp.GenerateKey(sideroRunIdentity, "", sideroRunIdentity+"@executor.invalid", lifetime)
	if err != nil {
		return nil, fmt.Errorf("sidero proxy: run key: %w", err)
	}
	b64, err := serviceaccount.Encode(sideroRunIdentity, key)
	if err != nil {
		return nil, fmt.Errorf("sidero proxy: run key: %w", err)
	}
	cert, caPEM, err := loopbackCert()
	if err != nil {
		return nil, fmt.Errorf("sidero proxy: certificate: %w", err)
	}
	if tool == ToolTalosctl {
		clusters = []string{cluster}
	}
	return &sideroProxy{
		tool: tool, cluster: cluster, clusters: slices.Clone(clusters), preflight: preflight, real: real, upstream: upstream,
		sensitivity: map[string]bool{},
		runName: sideroRunIdentity, runKey: key, runKeyB64: b64,
		tls:   &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		caPEM: caPEM,
	}, nil
}

// serve serves the proxy on ln until stop is called.
func (p *sideroProxy) serve(ln net.Listener) (stop func()) {
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(p.tls)),
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(p.handle),
		grpc.MaxRecvMsgSize(4<<20),
	)
	go func() { _ = srv.Serve(ln) }()
	return srv.Stop
}

// The methods the allowed commands call. Everything else is refused, by name: Omni's
// kubeconfig, talosconfig, service-account, user, join-token and support-bundle methods; Talos's
// file reads and copies, etcd snapshots, client-certificate minting, packet captures, config
// applies and META writes. Also the kernel log, through Talos (dmesg) or Omni (machine logs): it
// prints the kernel command line at boot, which may hold the SideroLink join token.
var (
	sideroReads = []string{
		"/machine.MachineService/Version", "/machine.MachineService/ServiceList", "/machine.MachineService/Logs",
		"/machine.MachineService/LogsContainers", "/machine.MachineService/EtcdMemberList",
		"/machine.MachineService/EtcdStatus", "/machine.MachineService/EtcdAlarmList", "/machine.MachineService/Containers",
		"/machine.MachineService/Stats", "/machine.MachineService/Memory", "/machine.MachineService/Processes",
		"/machine.MachineService/Mounts", "/machine.MachineService/Netstat", "/machine.MachineService/List",
		"/machine.MachineService/Hostname", "/machine.MachineService/Events",
		"/time.TimeService/Time", "/time.TimeService/TimeCheck",
		cosiState + "Get", cosiState + "List", cosiState + "Watch",
	}
	sideroWrites = []string{
		"/machine.MachineService/Reboot", "/machine.MachineService/Shutdown", "/machine.MachineService/Reset",
		"/machine.MachineService/Upgrade", methodImagePull, methodLifecycleUpgrade,
		"/machine.MachineService/Rollback", "/machine.MachineService/ServiceStart",
		"/machine.MachineService/ServiceStop", "/machine.MachineService/ServiceRestart",
		"/machine.MachineService/EtcdDefragment", "/machine.MachineService/EtcdForfeitLeadership",
		"/machine.MachineService/EtcdLeaveCluster", "/machine.MachineService/EtcdRemoveMemberByID",
		"/machine.MachineService/EtcdAlarmDisarm",
		cosiState + "Create", cosiState + "Update", cosiState + "Destroy", cosiState + "Teardown", cosiState + "TeardownAndDestroy",
	}
)

const cosiState = "/cosi.resource.State/"

// forwardHeaders are the request headers the proxy forwards: the ones Omni's signature covers,
// and talosctl's single-node header. The rest are dropped.
var forwardHeaders = []string{
	message.NodesHeaderKey, "node", message.SelectorsHeaderKey, message.FieldSelectorsHeaderKey, message.RuntimeHeaderKey,
	message.ContextHeaderKey, message.ClusterHeaderKey, message.NamespaceHeaderKey, message.UIDHeaderKey,
}

// talosHeaders route a request to Talos nodes through Omni.
var talosHeaders = []string{message.NodesHeaderKey, "node", message.ContextHeaderKey, message.ClusterHeaderKey}

func refusedBy(format string, a ...any) error {
	return grpcstatus.Error(codes.PermissionDenied, "refused by the executor: "+fmt.Sprintf(format, a...))
}

func (p *sideroProxy) handle(_ any, ss grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok {
		return grpcstatus.Error(codes.Internal, "no method")
	}
	md, _ := metadata.FromIncomingContext(ss.Context())
	if err := p.verify(md, method); err != nil {
		return grpcstatus.Error(codes.Unauthenticated, "not this run's signature")
	}
	if err := p.allowed(method, md); err != nil {
		return err
	}
	var req []byte
	if err := ss.RecvMsg(&req); err != nil {
		return err
	}
	var extra []byte
	if err := ss.RecvMsg(&extra); !errors.Is(err, io.EOF) {
		return refusedBy("one request message per call")
	}
	out := metadata.MD{}
	for _, k := range forwardHeaders {
		if v := md.Get(k); len(v) > 0 {
			out.Set(k, v...)
		}
	}
	if err := p.check(ss.Context(), method, out, req); err != nil {
		return err
	}
	call, filtered := strings.CutPrefix(method, cosiState)
	filtered = filtered && p.tool == ToolOmnictl
	return p.upstreamCall(ss.Context(), method, out, req, func(resp []byte) error {
		if filtered {
			var err error
			if resp, err = p.filter(call, resp); err != nil {
				if grpcstatus.Code(err) == codes.PermissionDenied {
					return err
				}
				return refusedBy("an answer from Omni cannot be read to check its cluster")
			}
			if resp == nil {
				return nil
			}
		}
		return ss.SendMsg(&resp)
	})
}

// upstreamCall makes one call upstream, signed with the executor's key, with the given headers,
// and passes each answer to recv.
func (p *sideroProxy) upstreamCall(ctx context.Context, method string, md metadata.MD, req []byte, recv func([]byte) error) error {
	msg := message.NewGRPC(md.Copy(), method)
	if err := msg.Sign(p.real.Name, p.real.Key); err != nil {
		return grpcstatus.Error(codes.Internal, "the executor could not sign the request")
	}
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(ctx, msg.Metadata))
	defer cancel()
	cs, err := p.upstream.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return err
	}
	if err := cs.SendMsg(&req); err != nil {
		return err
	}
	if err := cs.CloseSend(); err != nil {
		return err
	}
	for {
		var resp []byte
		if err := cs.RecvMsg(&resp); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if err := recv(resp); err != nil {
			return err
		}
	}
}

// verify checks the request carries the run key's valid, current signature.
func (p *sideroProxy) verify(md metadata.MD, method string) error {
	msg := message.NewGRPC(md.Copy(), method)
	sig, err := msg.Signature()
	if err != nil {
		return err
	}
	if sig.Identity != p.runName || sig.KeyFingerprint != p.runKey.Fingerprint() {
		return errors.New("another identity")
	}
	return msg.VerifySignature(p.runKey)
}

// allowed applies the method and header rules.
func (p *sideroProxy) allowed(method string, md metadata.MD) error {
	switch {
	case len(md.Get(message.AuthorizationHeaderKey)) > 0:
		return refusedBy("an authorization header")
	case slices.Contains(sideroReads, method):
	case slices.Contains(sideroWrites, method) && p.preflight:
		return refusedBy("%s writes, and this is the preview before approval", method)
	case slices.Contains(sideroWrites, method):
	default:
		return refusedBy("%s is not a method the executor forwards", method)
	}
	talos := slices.ContainsFunc(talosHeaders, func(k string) bool { return len(md.Get(k)) > 0 })
	switch {
	case p.tool == ToolOmnictl && talos:
		return refusedBy("omnictl reaches Omni only, not Talos nodes")
	case p.tool == ToolTalosctl && !talos:
		return refusedBy("talosctl reaches its cluster's nodes only, not Omni")
	case p.tool == ToolTalosctl && !slices.Equal(md.Get(message.ContextHeaderKey), []string{p.cluster}):
		return refusedBy("the request is not for cluster %s", p.cluster)
	case p.tool == ToolTalosctl && len(md.Get(message.ClusterHeaderKey)) > 0 && !slices.Equal(md.Get(message.ClusterHeaderKey), []string{p.cluster}):
		return refusedBy("the request is not for cluster %s", p.cluster)
	}
	return nil
}

// cosiTarget reads the namespace and type a COSI State request names, from its protobuf
// encoding: fields 1 and 2 of the request, or of the metadata (field 1) of its resource (Create's
// field 1, Update's field 2). A field the proxy reads that is repeated, or of another wire type, is
// refused: protobuf readers disagree on which of two values wins.
func cosiTarget(call string, body []byte) (ns, typ string, err error) {
	switch call {
	case "Get", "List", "Watch", "Destroy", "Teardown", "TeardownAndDestroy":
	case "Create", "Update":
		field := protowire.Number(1)
		if call == "Update" {
			field = 2
		}
		res, err := protoField(body, field)
		if err != nil {
			return "", "", err
		}
		if body, err = protoField(res, 1); err != nil {
			return "", "", err
		}
	default:
		return "", "", errors.New("not a State call the executor reads")
	}
	nsb, err := protoField(body, 1)
	if err != nil {
		return "", "", err
	}
	tb, err := protoField(body, 2)
	if err != nil {
		return "", "", err
	}
	if len(tb) == 0 {
		return "", "", errors.New("no resource type")
	}
	return string(nsb), string(tb), nil
}

// protoField returns the bytes of a length-delimited field that appears at most once, or nil.
func protoField(b []byte, want protowire.Number) ([]byte, error) {
	var out []byte
	seen := false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, errors.New("the request cannot be read")
		}
		b = b[n:]
		if num == want {
			if seen || typ != protowire.BytesType {
				return nil, errors.New("the request repeats or retypes a field the executor reads")
			}
			seen = true
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return nil, errors.New("the request cannot be read")
		}
		if num == want {
			out, _ = protowire.ConsumeBytes(b[:m])
		}
		b = b[m:]
	}
	return out, nil
}

// rawCodec passes messages through as bytes.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("raw codec: %T", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("raw codec: %T", v)
	}
	*b = slices.Clone(data)
	return nil
}

func (rawCodec) Name() string { return "proto" }

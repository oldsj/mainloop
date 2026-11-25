package privileged

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/siderolabs/go-api-signature/pkg/message"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"sigs.k8s.io/yaml"
)

// What the proxy reads from Omni or Talos itself, with the executor's key, to decide a request:
// a type's resource definition (is it marked sensitive), the machines of the clusters it may act
// on (is a node one of them), and an Omni resource's cluster (may it be changed, or shown). The
// tool never sees these answers.

const (
	cosiResourceDefinitions = "ResourceDefinitions.meta.cosi.dev"
	omniClusterMachineIDs   = "ClusterMachineIdentities.omni.sidero.dev"
	omniDefaultNamespace    = "default"
	methodUpgrade           = "/machine.MachineService/Upgrade"
	// talosctl upgrade on Talos 1.13 and later pulls the image, then installs it, then reboots.
	methodImagePull        = "/machine.ImageService/Pull"
	methodLifecycleUpgrade = "/machine.LifecycleService/Upgrade"
	// ResourceDefinitionSpec's sensitivity field, and its SENSITIVE value.
	rdSensitivityField = 7
	rdSensitive        = 1
)

// cosiWrites are the State calls that change a resource.
var cosiWrites = []string{"Create", "Update", "Destroy", "Teardown", "TeardownAndDestroy"}

// check applies the rules that read the request's body, and those that ask Omni. md is the
// headers the proxy forwards.
func (p *sideroProxy) check(ctx context.Context, method string, md metadata.MD, req []byte) error {
	if p.tool == ToolTalosctl {
		if err := p.checkNodes(ctx, md); err != nil {
			return err
		}
	}
	switch {
	case strings.HasPrefix(method, cosiState):
		return p.checkState(ctx, strings.TrimPrefix(method, cosiState), md, req)
	case method == methodUpgrade, method == methodImagePull, method == methodLifecycleUpgrade:
		image, err := upgradeImage(method, req)
		if err != nil {
			return refusedBy("%s: %v", method, err)
		}
		// The legacy Upgrade with no image installs the node's own default; the others need one.
		if (len(image) > 0 || method != methodUpgrade) && !talosImage(string(image)) {
			return refusedBy("the upgrade image is not from factory.talos.dev or ghcr.io/siderolabs")
		}
	}
	return nil
}

// upgradeImage is the image an upgrade call installs: machine.UpgradeRequest.image (1),
// ImageServicePullRequest.image_ref (3), or LifecycleServiceUpgradeRequest.source (2)
// .image_name (1).
func upgradeImage(method string, req []byte) ([]byte, error) {
	switch method {
	case methodImagePull:
		return protoField(req, 3)
	case methodLifecycleUpgrade:
		source, err := protoField(req, 2)
		if err != nil || source == nil {
			return nil, err
		}
		return protoField(source, 1)
	}
	return protoField(req, 1)
}

func (p *sideroProxy) checkState(ctx context.Context, call string, md metadata.MD, req []byte) error {
	ns, typ, err := cosiTarget(call, req)
	if err != nil {
		return refusedBy("%s: %v", call, err)
	}
	write := slices.Contains(cosiWrites, call)
	switch {
	case !sideroReadable(p.tool, typ):
		return refusedBy("%s is not a resource type the executor reads or writes", typ)
	case secretType(typ) || slices.Contains(secretNamespaces, ns):
		return refusedBy("%s may hold secrets", typ)
	case write && (p.tool == ToolTalosctl || slices.Contains(metaTypes, typ) || slices.Contains(omniSystemTypes, typ)):
		return refusedBy("%s is never written", typ)
	}
	if !slices.Contains(metaTypes, typ) {
		sensitive, err := p.sensitive(ctx, md, typ)
		switch {
		case err != nil:
			return refusedBy("the resource definition of %s cannot be read, so it is not known not to be sensitive", typ)
		case sensitive:
			return refusedBy("%s is marked sensitive", typ)
		}
	}
	if write {
		return p.checkOmniWrite(ctx, call, req)
	}
	return nil
}

// sensitive asks the server (Omni, or the node md routes to) whether its resource definition of
// typ marks it sensitive.
func (p *sideroProxy) sensitive(ctx context.Context, md metadata.MD, typ string) (bool, error) {
	key := strings.Join(md.Get(message.NodesHeaderKey), ",") + "|" + strings.Join(md.Get("node"), ",") + "|" + typ
	p.mu.Lock()
	v, ok := p.sensitivity[key]
	p.mu.Unlock()
	if ok {
		return v, nil
	}
	res, err := p.get(ctx, md, metaNamespace, cosiResourceDefinitions, strings.ToLower(typ))
	if err != nil {
		return false, err
	}
	v, err = definitionSensitive(res)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	p.sensitivity[key] = v
	p.mu.Unlock()
	return v, nil
}

const metaNamespace = "meta"

// definitionSensitive reads a ResourceDefinition's sensitivity from its protobuf spec, or its YAML
// spec when it has none.
func definitionSensitive(r cosiResource) (bool, error) {
	if !strings.EqualFold(r.typ, cosiResourceDefinitions) {
		return false, errors.New("not a resource definition")
	}
	if len(r.protoSpec) > 0 {
		v, found, err := protoVarint(r.protoSpec, rdSensitivityField)
		if err != nil {
			return false, err
		}
		return found && v == rdSensitive, nil
	}
	if len(r.yamlSpec) > 0 {
		var spec struct {
			Sensitivity any `json:"sensitivity"`
		}
		if err := yaml.Unmarshal(r.yamlSpec, &spec); err != nil {
			return false, err
		}
		switch s := spec.Sensitivity.(type) {
		case nil:
			return false, nil
		case string:
			return s != "", nil
		case float64:
			return s != 0, nil
		}
		return true, nil
	}
	return false, errors.New("the resource definition has no spec")
}

// clusterMachines are the machines of the clusters the proxy may act on: their IDs, node names
// and addresses.
type clusterMachines struct {
	ids   map[string]bool
	names map[string]bool
	addrs map[netip.Addr]bool
}

func (m clusterMachines) has(node string) bool {
	if a, err := netip.ParseAddr(strings.Trim(node, "[]")); err == nil {
		return m.addrs[a.Unmap()]
	}
	return m.ids[node] || m.names[strings.ToLower(node)]
}

// machines lists Omni's ClusterMachineIdentities for the proxy's clusters, once per run.
func (p *sideroProxy) machines(ctx context.Context) (clusterMachines, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clusterMachines != nil {
		return *p.clusterMachines, nil
	}
	m := clusterMachines{ids: map[string]bool{}, names: map[string]bool{}, addrs: map[netip.Addr]bool{}}
	err := p.upstreamCall(ctx, cosiState+"List", metadata.MD{}, cosiList(omniDefaultNamespace, omniClusterMachineIDs), func(msg []byte) error {
		b, err := protoField(msg, 1)
		if err != nil || b == nil {
			return errors.New("a machine identity cannot be read")
		}
		r, err := parseCosiResource(b)
		if err != nil {
			return err
		}
		if c, ok := r.labels[omniClusterLabel]; !ok || !slices.Contains(p.clusters, c) {
			return nil
		}
		m.ids[r.id] = true
		if name, err := protoField(r.protoSpec, 3); err == nil && len(name) > 0 { // nodename
			m.names[strings.ToLower(string(name))] = true
		}
		return protoEach(r.protoSpec, 8, func(ip []byte) error { // node_ips
			if a, err := netip.ParseAddr(string(ip)); err == nil {
				m.addrs[a.Unmap()] = true
			}
			return nil
		})
	})
	if err != nil {
		return m, refusedBy("the machines of %s cannot be read from Omni", strings.Join(p.clusters, ", "))
	}
	p.clusterMachines = &m
	return m, nil
}

// checkNodes refuses a talosctl request for a node that is not a machine of its cluster: Omni
// routes a node given by address, so the context header alone does not hold it to the cluster.
func (p *sideroProxy) checkNodes(ctx context.Context, md metadata.MD) error {
	var nodes []string
	for _, k := range []string{message.NodesHeaderKey, "node"} {
		for _, v := range md.Get(k) {
			nodes = append(nodes, strings.Split(v, ",")...)
		}
	}
	if len(nodes) == 0 {
		return refusedBy("the request names no node")
	}
	m, err := p.machines(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if !m.has(strings.TrimSpace(n)) {
			return refusedBy("node %q is not a machine of cluster %s in Omni", n, p.cluster)
		}
	}
	return nil
}

// checkOmniWrite holds an omnictl write to the clusters the executor acts on: the resource it
// creates, the one it replaces and what replaces it, or the one it destroys, must belong to one
// of them. A write to a resource of no cluster (a machine class, a machine's labels) is refused.
func (p *sideroProxy) checkOmniWrite(ctx context.Context, call string, req []byte) error {
	var check []cosiResource
	switch call {
	case "Create", "Update":
		field := protowire.Number(1)
		if call == "Update" {
			field = 2
		}
		b, err := protoField(req, field)
		if err != nil {
			return refusedBy("%s: %v", call, err)
		}
		r, err := parseCosiResource(b)
		if err != nil {
			return refusedBy("%s: %v", call, err)
		}
		check = append(check, r)
		if call == "Update" {
			cur, err := p.get(ctx, metadata.MD{}, r.ns, r.typ, r.id)
			if err != nil {
				return refusedBy("%s %s cannot be read to check its cluster", r.typ, r.id)
			}
			check = append(check, cur)
		}
	default: // Destroy, Teardown, TeardownAndDestroy: namespace, type, id
		ns, err1 := protoField(req, 1)
		typ, err2 := protoField(req, 2)
		id, err3 := protoField(req, 3)
		if err := errors.Join(err1, err2, err3); err != nil {
			return refusedBy("%s: %v", call, err)
		}
		cur, err := p.get(ctx, metadata.MD{}, string(ns), string(typ), string(id))
		if err != nil {
			return refusedBy("%s %s cannot be read to check its cluster", typ, id)
		}
		check = append(check, cur)
	}
	for _, r := range check {
		c, ok := omniClusterOf(r.typ, r.id, r.labels)
		switch {
		case !ok:
			return refusedBy("%s %s belongs to no cluster; the executor changes only resources of %s", r.typ, r.id, strings.Join(p.clusters, ", "))
		case !slices.Contains(p.clusters, c):
			return refusedBy("%s %s belongs to cluster %q, which the executor does not act on", r.typ, r.id, c)
		}
	}
	return nil
}

// visible says whether omnictl may see an Omni resource: not one of a cluster the executor does
// not act on.
func (p *sideroProxy) visible(r cosiResource) bool {
	c, ok := omniClusterOf(r.typ, r.id, r.labels)
	return !ok || slices.Contains(p.clusters, c)
}

// filter applies visible to one response of a State read: a Get's resource, a List's, or a
// Watch's events (with their previous versions). It returns nil to drop the message.
func (p *sideroProxy) filter(call string, msg []byte) ([]byte, error) {
	shown := func(b []byte) (bool, error) {
		if b == nil {
			return true, nil
		}
		r, err := parseCosiResource(b)
		if err != nil {
			return false, err
		}
		return p.visible(r), nil
	}
	switch call {
	case "Get", "List":
		b, err := protoField(msg, 1)
		if err != nil {
			return nil, err
		}
		ok, err := shown(b)
		switch {
		case err != nil:
			return nil, err
		case !ok && call == "Get":
			return nil, refusedBy("the resource belongs to a cluster the executor does not act on")
		case !ok:
			return nil, nil
		}
		return msg, nil
	case "Watch":
		var out []byte
		kept, events := 0, 0
		err := protoRange(msg, func(num protowire.Number, raw, value []byte) error {
			if num != 1 {
				out = append(out, raw...)
				return nil
			}
			events++
			res, err1 := protoField(value, 1)
			old, err2 := protoField(value, 3)
			if err := errors.Join(err1, err2); err != nil {
				return err
			}
			a, err1 := shown(res)
			b, err2 := shown(old)
			if err := errors.Join(err1, err2); err != nil {
				return err
			}
			if a && b {
				out = append(out, raw...)
				kept++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if events > 0 && kept == 0 {
			return nil, nil
		}
		return out, nil
	}
	return msg, nil
}

// get reads one resource from upstream with the executor's key.
func (p *sideroProxy) get(ctx context.Context, md metadata.MD, ns, typ, id string) (cosiResource, error) {
	var res cosiResource
	found := false
	err := p.upstreamCall(ctx, cosiState+"Get", md, cosiGet(ns, typ, id), func(msg []byte) error {
		b, err := protoField(msg, 1)
		if err != nil || b == nil {
			return errors.New("no resource in the answer")
		}
		res, err = parseCosiResource(b)
		found = err == nil
		return err
	})
	if err == nil && !found {
		err = grpcstatus.Error(codes.NotFound, "not found")
	}
	return res, err
}

// cosiResource is what the proxy reads of a COSI resource.
type cosiResource struct {
	ns, typ, id string
	labels      map[string]string
	protoSpec   []byte
	yamlSpec    []byte
}

// parseCosiResource reads a cosi.resource.Resource: metadata (1) with namespace (1), type (2),
// id (3) and labels (10, a map); spec (2) with proto_spec (1) and yaml_spec (2).
func parseCosiResource(b []byte) (cosiResource, error) {
	r := cosiResource{labels: map[string]string{}}
	md, err1 := protoField(b, 1)
	spec, err2 := protoField(b, 2)
	if err := errors.Join(err1, err2); err != nil {
		return r, err
	}
	ns, err1 := protoField(md, 1)
	typ, err2 := protoField(md, 2)
	id, err3 := protoField(md, 3)
	if err := errors.Join(err1, err2, err3); err != nil {
		return r, err
	}
	if len(typ) == 0 {
		return r, errors.New("a resource has no type")
	}
	r.ns, r.typ, r.id = string(ns), string(typ), string(id)
	err := protoEach(md, 10, func(entry []byte) error {
		k, err1 := protoField(entry, 1)
		v, err2 := protoField(entry, 2)
		if err := errors.Join(err1, err2); err != nil {
			return err
		}
		if _, dup := r.labels[string(k)]; dup {
			return fmt.Errorf("label %q is given twice", k)
		}
		r.labels[string(k)] = string(v)
		return nil
	})
	if err != nil {
		return r, err
	}
	if r.protoSpec, err = protoField(spec, 1); err != nil {
		return r, err
	}
	r.yamlSpec, err = protoField(spec, 2)
	return r, err
}

func cosiGet(ns, typ, id string) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendString(b, ns)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, typ)
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	return protowire.AppendString(b, id)
}

func cosiList(ns, typ string) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendString(b, ns)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendString(b, typ)
}

// protoRange calls fn with each field of b: its number, its raw bytes (tag and value), and, for
// a length-delimited field, its value.
func protoRange(b []byte, fn func(num protowire.Number, raw, value []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errors.New("the message cannot be read")
		}
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if m < 0 {
			return errors.New("the message cannot be read")
		}
		var value []byte
		if typ == protowire.BytesType {
			value, _ = protowire.ConsumeBytes(b[n : n+m])
		}
		if err := fn(num, b[:n+m], value); err != nil {
			return err
		}
		b = b[n+m:]
	}
	return nil
}

// protoEach calls fn with each value of a repeated length-delimited field.
func protoEach(b []byte, want protowire.Number, fn func([]byte) error) error {
	return protoRange(b, func(num protowire.Number, raw, value []byte) error {
		if num != want {
			return nil
		}
		if _, typ, _ := protowire.ConsumeTag(raw); typ != protowire.BytesType {
			return errors.New("a field the executor reads has another wire type")
		}
		return fn(value)
	})
}

// protoVarint reads a varint field that appears at most once.
func protoVarint(b []byte, want protowire.Number) (v uint64, found bool, err error) {
	err = protoRange(b, func(num protowire.Number, raw, _ []byte) error {
		if num != want {
			return nil
		}
		_, typ, n := protowire.ConsumeTag(raw)
		if found || typ != protowire.VarintType {
			return errors.New("a field the executor reads repeats or has another wire type")
		}
		found = true
		v, _ = protowire.ConsumeVarint(raw[n:])
		return nil
	})
	return v, found, err
}

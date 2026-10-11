package workspaces

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func (h *harness) knownAgents() []string {
	h.t.Helper()
	rows, err := h.dq.ListKnownAgents(bg)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Namespace+"/"+r.Name)
	}
	slices.Sort(out)
	return out
}

// The sweep reports the Agent set it covers, and the command prints it ahead of the orphans.
func TestSweepOrphansReportsTheAgentsItSweeps(t *testing.T) {
	h := newHarness(t, nil)
	h.exec(`INSERT INTO kagent_agents (namespace, name) VALUES ('earlier', 'earlier-agent')`)
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.svc.configAgents()
	if !slices.Contains(rep.Agents, "earlier/earlier-agent") || len(rep.Agents) != len(cfg)+1 || !slices.IsSorted(rep.Agents) {
		t.Fatalf("agents %v, config %v", rep.Agents, cfg)
	}
	var out, errb bytes.Buffer
	if code := printOrphans(rep, nil, false, &out, &errb); code != 0 || !strings.Contains(out.String(), "agent   earlier/earlier-agent\n") {
		t.Fatalf("code %d out %q", code, out.String())
	}
}

// --forget-agent drops an Agent a deployment once used: its Sessions are no longer swept. It
// refuses an Agent the configuration names, one that is not recorded, and a malformed name.
func TestForgetAgentRemovesItFromTheSweptSet(t *testing.T) {
	h := newHarness(t, nil)
	h.fake.AddAgent("earlier", "earlier-agent")
	h.exec(`INSERT INTO kagent_agents (namespace, name) VALUES ('earlier', 'earlier-agent')`)
	orphan := h.strayOn("earlier", "earlier-agent", "stray-earlier")
	h.svc.Now = func() time.Time { return time.Now().Add(time.Hour) }

	var configured string
	for k := range h.svc.configAgents() {
		configured = k
		break
	}
	h.exec(`INSERT INTO kagent_agents (namespace, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, strings.Split(configured, "/")[0], strings.Split(configured, "/")[1])
	before := h.knownAgents()
	for _, bad := range []string{configured, "nobody/nothing", "earlier", "/x", "a/b/c"} {
		var out, errb bytes.Buffer
		if code := forgetAgent(bg, h.svc, bad, &out, &errb); code != 1 || out.Len() != 0 || errb.Len() == 0 {
			t.Fatalf("%q: code %d out %q err %q", bad, code, out.String(), errb.String())
		}
	}
	if got := h.knownAgents(); !slices.Equal(got, before) {
		t.Fatalf("a refused forget changed the set: %v -> %v", before, got)
	}

	var out, errb bytes.Buffer
	if code := forgetAgent(bg, h.svc, "earlier/earlier-agent", &out, &errb); code != 0 || out.String() != "forgot agent earlier/earlier-agent\n" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
	if slices.Contains(h.knownAgents(), "earlier/earlier-agent") {
		t.Fatal("the Agent is still recorded")
	}
	rep, err := h.svc.SweepOrphans(bg, OrphanOptions{Delete: true})
	if err != nil || slices.Contains(rep.Agents, "earlier/earlier-agent") || len(rep.Orphans) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	if s, ok := h.fake.Session(orphan); !ok || s.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_DELETED {
		t.Fatalf("the forgotten Agent's Session was touched: %v %v", ok, s)
	}
}

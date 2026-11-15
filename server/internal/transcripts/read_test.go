package transcripts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/api"
)

func TestReadPagesStoredEntries(t *testing.T) {
	e := newIngest(t, Options{}, a2a.TaskStateCompleted, a2a.TaskStateCompleted, a2a.TaskStateCompleted)
	e.ingest(t)
	ctx := context.Background()

	first, err := e.svc.Read(ctx, ReadRequest{SessionID: session, Limit: 4, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 4 || !first.HasMore || len(first.Live) != 0 || first.LiveError != nil {
		t.Fatalf("first page: %+v", first)
	}
	if first.Entries[0].Kind != api.TranscriptEntryKindUser || first.Entries[1].Kind != api.TranscriptEntryKindAssistant {
		t.Fatalf("order: %+v", first.Entries[:2])
	}
	if len(e.k.tokens) != 1 {
		t.Fatal("a page short of the end read kagent")
	}
	if len(first.Sources) != 1 || first.Sources[0].State != api.TranscriptSourceStateActive || first.Sources[0].StoredBytes == 0 {
		t.Fatalf("sources: %+v", first.Sources)
	}

	rest, err := e.svc.Read(ctx, ReadRequest{SessionID: session, After: *first.Entries[3].Id, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Entries) != 2 || rest.HasMore || *rest.Entries[0].Id <= *first.Entries[3].Id {
		t.Fatalf("second page: %+v", rest)
	}
}

func TestReadOwnerScoped(t *testing.T) {
	e := newIngest(t, Options{})
	e.svc.OwnerID = "someone-else"
	if _, err := e.svc.Read(context.Background(), ReadRequest{SessionID: session}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestReadLiveTail(t *testing.T) {
	// t0 is copied; t1 is running; t2 finished after the pass and is not copied yet.
	e := newIngest(t, Options{}, a2a.TaskStateCompleted, a2a.TaskStateWorking)
	e.ingest(t)
	e.k.tasks = append(e.k.tasks, task("t2", a2a.TaskStateCompleted))
	e.k.tasks[1].Artifacts = append(e.k.tasks[1].Artifacts,
		artifact("t1-r", 20, toolResult("c1", "Bash", "token=zebracornflakes\nbuilding", false)))
	ctx := context.Background()

	out, err := e.svc.Read(ctx, ReadRequest{SessionID: session, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) != 2 || out.LiveError != nil {
		t.Fatalf("%+v", out)
	}
	var tasks []string
	for _, l := range out.Live {
		if !l.Live || l.Id != nil {
			t.Fatalf("live entry %+v", l)
		}
		if strings.Contains(l.Text, "zebracornflakes") {
			t.Fatal("the live tail is not redacted")
		}
		if len(tasks) == 0 || tasks[len(tasks)-1] != l.TaskId {
			tasks = append(tasks, l.TaskId)
		}
	}
	if strings.Join(tasks, ",") != "t1,t2" {
		t.Fatalf("live tasks %v", tasks)
	}
	if e.k.gets["t0"] != 1 {
		t.Fatal("the tail read a copied task")
	}

	// Without live, kagent is not read.
	lists := len(e.k.tokens)
	if out, err := e.svc.Read(ctx, ReadRequest{SessionID: session}); err != nil || len(out.Live) != 0 || len(e.k.tokens) != lists {
		t.Fatalf("live off: %+v %v", out, err)
	}

	// kagent failing leaves the stored page whole and says why, without its error text.
	e.k.listErr = errors.New("dial tcp 10.0.0.1: secret-ish detail")
	out, err = e.svc.Read(ctx, ReadRequest{SessionID: session, Live: true})
	if err != nil || len(out.Entries) != 2 || out.LiveError == nil || strings.Contains(*out.LiveError, "10.0.0.1") {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestSearchFiltersAndTitles(t *testing.T) {
	e := newIngest(t, Options{}, a2a.TaskStateCompleted)
	e.ingest(t)
	ctx := context.Background()
	if _, err := e.d.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p1', $1, 'o', 'p', 'o/p', 'https://x')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Pool.Exec(ctx, `UPDATE sessions SET project_id = 'p1', title = 'Fix the build' WHERE id = $1`, session); err != nil {
		t.Fatal(err)
	}
	p1, other := "p1", "p2"
	out, err := e.svc.Search(ctx, SearchRequest{Query: "answer", ProjectID: &p1})
	if err != nil || len(out.Hits) != 1 || out.Hits[0].SessionTitle != "Fix the build" || out.Hits[0].Kind != api.TranscriptEntryKindAssistant {
		t.Fatalf("%+v %v", out, err)
	}
	if out, _ := e.svc.Search(ctx, SearchRequest{Query: "answer", ProjectID: &other}); len(out.Hits) != 0 {
		t.Fatalf("other project: %+v", out)
	}
	if out, _ := e.svc.Search(ctx, SearchRequest{Query: "answer", SessionID: &other}); len(out.Hits) != 0 {
		t.Fatalf("other session: %+v", out)
	}
	var matched []string
	for _, p := range out.Hits[0].Snippet {
		if p.Match {
			matched = append(matched, p.Text)
		}
	}
	if len(matched) != 1 || matched[0] != "answer" {
		t.Fatalf("snippet %+v", out.Hits[0].Snippet)
	}
}

func TestSnippetIsPlainText(t *testing.T) {
	got := Snippet("run <script>x</script> \x02deploy\x03 now \x02ok\x03")
	want := []api.TranscriptSnippetPart{
		{Text: "run <script>x</script> "}, {Text: "deploy", Match: true}, {Text: " now "}, {Text: "ok", Match: true},
	}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%+v", got)
		}
	}
	if long := Snippet(strings.Repeat("\x02a\x03b", 100)); len(long) != maxSnippetParts {
		t.Fatalf("%d parts", len(long))
	}
}

package spec

import (
	"os"
	"strings"
	"testing"
)

func TestCollections(t *testing.T) {
	ws, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.NewCollection("ai"); err != nil {
		t.Fatal(err)
	}
	if err := ws.NewCollection("default"); err == nil {
		t.Fatal("default collection must not be creatable")
	}
	os.WriteFile(ws.RequestPath("orphan"), []byte("url: https://x\n"), 0o644)
	os.WriteFile(ws.FlowPath("f"), []byte("steps:\n  - request: orphan\n  - request: 'httpbin/get'   # keep\n"), 0o644)

	// _collection.yaml must not show up as a request.
	for _, it := range ws.Items() {
		if strings.Contains(it.Name, "_collection") {
			t.Fatalf("settings file listed as %s", it.Name)
		}
	}
	it, _ := ws.Lookup("orphan")
	newName, err := ws.MoveRequest(it, "ai")
	if err != nil || newName != "ai/orphan" {
		t.Fatalf("move: %v %s", err, newName)
	}
	if r, _ := ws.LoadRequest("ai/orphan"); r.Collection != "ai" {
		t.Fatalf("collection = %q", r.Collection)
	}
	n, err := ws.RenameCollection("httpbin", "bin")
	if err != nil || n != 2 { // f and the seeded example-pipeline
		t.Fatalf("rename: %v, %d flows", err, n)
	}
	b, _ := os.ReadFile(ws.FlowPath("f"))
	want := "steps:\n  - request: ai/orphan\n  - request: 'bin/get'   # keep\n"
	if string(b) != want {
		t.Fatalf("flow refs not updated:\n%s", b)
	}
	if c, _ := ws.LoadCollection("bin"); c.Vars["base_url"] == "" {
		t.Fatal("settings did not move with the collection")
	}
	if err := ws.DeleteCollection(""); err == nil {
		t.Fatal("default collection must not be deletable")
	}
	if err := ws.DeleteCollection("ai"); err != nil || ws.CollectionExists("ai") {
		t.Fatalf("delete: %v", err)
	}
}

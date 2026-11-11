package proxy

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ownerCheckForgetDriver runs forget of scripts/iwdp-owner-check.sh (the
// removal of 'iwdp-owner-check.sh --forget' after "gone") in a test
// process of its own, on scenarios that it reads as JSON names from
// stdin. For each, it writes the record iwdp.pid in a new directory,
// reads it once as the helper does (read_file), lets another program of
// this user change the path as the scenario says, calls forget with the
// one read, and answers the result code, the reason, the directory and
// the content of every file in it. The changes after the move replace
// os.rename inside this process only.
const ownerCheckForgetDriver = `import json, os, sys, tempfile
sys.path.insert(0, sys.argv[1])
import owner_check as oc

ORIGINAL, OTHER, THIRD = b"4242 tok-a Mon Nov 10 10:00:00 2026\n", b"4343 tok-b Mon Nov 10 10:00:00 2026\n", b"4444 tok-c Mon Nov 10 10:00:00 2026\n"
real_rename = os.rename

def put(path, data):
    """A new file at path, by rename, as the record writers do."""
    with open(path + ".new", "wb") as f:
        f.write(data)
    real_rename(path + ".new", path)

def overwrite(path, data):
    """The same file with new content (a write in place)."""
    with open(path, "r+b") as f:
        f.truncate(0)
        f.write(data)

def taken_after_move(data):
    """os.rename that moves the record, after which a new record takes the path."""
    def rename(src, dst):
        real_rename(src, dst)
        put(src, data)
    return rename

answer = []
for name in json.load(sys.stdin):
    d = tempfile.mkdtemp()
    path = os.path.join(d, "iwdp.pid")
    put(path, ORIGINAL)
    data, st = oc.read_file(path)
    if name == "in place":
        overwrite(path, OTHER)
    elif name == "same content, another file":
        put(path, ORIGINAL)
    elif name == "removed":
        os.unlink(path)
    elif name == "taken after the move":
        os.rename = taken_after_move(OTHER)
    elif name == "in place, then taken after the move":
        overwrite(path, OTHER)
        os.rename = taken_after_move(THIRD)
    elif name != "unchanged":
        raise SystemExit("unknown scenario " + name)
    try:
        oc.forget(path, data, st)
        result = {"code": None, "reason": "forget returned without a result"}
    except oc.Result as r:
        result = {"code": r.code, "reason": r.reason}
    finally:
        os.rename = real_rename
    result["dir"] = d
    result["files"] = {n: open(os.path.join(d, n), "rb").read().decode() for n in os.listdir(d)}
    answer.append(result)
json.dump(answer, sys.stdout)
`

// TestOwnerCheckForgetRemovesOnlyTheRecordItRead: after "gone",
// 'iwdp-owner-check.sh --forget' removes the record only when the path
// still names exactly the record of its one read, the same file with the
// same content (review WS3 round 5, r2: the teardown removed a record
// that had replaced the one it checked). It moves the path aside before
// it compares, so a record that takes the path in the meantime stays. On
// a mismatch the file goes back to the path only when the path is free;
// otherwise it stays under its private name, which the reason gives.
func TestOwnerCheckForgetRemovesOnlyTheRecordItRead(t *testing.T) {
	const (
		original = "4242 tok-a Mon Nov 10 10:00:00 2026\n"
		other    = "4343 tok-b Mon Nov 10 10:00:00 2026\n"
		third    = "4444 tok-c Mon Nov 10 10:00:00 2026\n"
		gone     = 3
		cannot   = 2
	)
	cases := []struct {
		name     string
		wantCode int
		wantWhy  string
		wantPath string // the content at iwdp.pid; "" for no file
		wantKept string // the content of the file kept under a private name; "" for none
	}{
		{name: "unchanged", wantCode: gone, wantWhy: "the record was removed"},
		{name: "in place", wantCode: cannot, wantWhy: "changed after it was read; it was kept", wantPath: other},
		{name: "same content, another file", wantCode: cannot, wantWhy: "changed after it was read; it was kept", wantPath: original},
		{name: "removed", wantCode: gone, wantWhy: "no longer exists"},
		{name: "taken after the move", wantCode: gone, wantWhy: "the record was removed", wantPath: other},
		{name: "in place, then taken after the move", wantCode: cannot, wantWhy: "another file took its path; it was kept as ", wantPath: third, wantKept: other},
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("the iwdp-owner-check.sh tests need python3: %v", err)
	}
	names := make([]string, len(cases))
	for i, tc := range cases {
		names[i] = tc.name
	}
	req, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), python, "-c", ownerCheckForgetDriver, ownerCheckPython(t))
	cmd.Stdin = strings.NewReader(string(req))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the forget driver: %v\n%s", err, stderr.String())
	}
	var answer []struct {
		Code   *int
		Reason string
		Dir    string
		Files  map[string]string
	}
	if err := json.Unmarshal(out, &answer); err != nil || len(answer) != len(cases) {
		t.Fatalf("the forget driver answered %q (%v); want %d results", out, err, len(cases))
	}
	for i, tc := range cases {
		got := answer[i]
		if got.Code == nil || *got.Code != tc.wantCode || !strings.Contains(got.Reason, tc.wantWhy) {
			code := "none"
			if got.Code != nil {
				code = strconv.Itoa(*got.Code)
			}
			t.Errorf("%s: code %s, reason %q; want code %d and a reason that says %q", tc.name, code, got.Reason, tc.wantCode, tc.wantWhy)
		}
		files := got.Files
		if content, ok := files["iwdp.pid"]; ok != (tc.wantPath != "") || content != tc.wantPath {
			t.Errorf("%s: iwdp.pid holds %q (exists %v); want %q", tc.name, content, ok, tc.wantPath)
		}
		delete(files, "iwdp.pid")
		if tc.wantKept == "" {
			if len(files) != 0 {
				t.Errorf("%s: files left besides iwdp.pid: %q", tc.name, files)
			}
			continue
		}
		if len(files) != 1 {
			t.Errorf("%s: files besides iwdp.pid: %q; want one private file with %q", tc.name, files, tc.wantKept)
			continue
		}
		for name, content := range files {
			kept := filepath.Join(got.Dir, name)
			if !strings.HasPrefix(name, ".iwdp.pid.forget-") || content != tc.wantKept || !strings.HasSuffix(got.Reason, "kept as "+kept) {
				t.Errorf("%s: kept %s with %q, reason %q; want a private name .iwdp.pid.forget-*, the content %q and the reason naming %s", tc.name, name, content, got.Reason, tc.wantKept, kept)
			}
		}
	}
}

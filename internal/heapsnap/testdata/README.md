# Heap snapshot fixtures

`webkitgtk-v3.json` is a real `Heap.snapshot` of WebKitGTK 2.54.3 (webkit2gtk-4.1, format version 3), trimmed. It was captured by

```bash
AGENT_IOS_HEAP_FIXTURE_OUT=/tmp/heap045-raw.json \
  go test -tags=webkitgtk ./e2e/webkitgtk/ -run TestHeapSnapshotSummaryAndDiff -count=1
```

on `forms.html` holding 60000 instances of the class `HeapProbe045` (3.1 MB, 65002 nodes, 134983 edges). The trim kept the `<root>` node, the first 40 `HeapProbe045` nodes and the first 5 nodes of every other class (in file order), the edges between kept nodes, and every other member as it was:

```python
import json, sys
d = json.load(open(sys.argv[1]))
names = d["nodeClassNames"]
nodes = d["nodes"]
keep, per = [nodes[0:4]], {}
for i in range(4, len(nodes), 4):
    n = nodes[i:i+4]
    name = names[n[2]]
    limit = 40 if name == "HeapProbe045" else 5
    if per.get(name, 0) < limit:
        per[name] = per.get(name, 0) + 1
        keep.append(n)
ids = {n[0] for n in keep}
edges = d["edges"]
kept_edges = [x for i in range(0, len(edges), 4) for x in edges[i:i+4] if edges[i] in ids and edges[i+1] in ids]
d["nodes"] = [x for n in keep for x in n]
d["edges"] = kept_edges
json.dump(d, open(sys.argv[2], "w"), separators=(",", ":"))
```

The expected summary in heapsnap_test.go was computed from the trimmed file by Web Inspector's class summary (HeapSnapshot.js `updateCategoriesAndMetadata`: skip the root, count and sum the self size by class name) in Python, not by this package.

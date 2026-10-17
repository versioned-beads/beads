# Read Beads from Python over BDP

This small example reads the BDP HTTP interface. It does not invoke `bd`, read
CLI JSON, or access a database. Python 3.9 or newer is enough; no packages need
installing. It implements the BDP v0 Read shapes pinned by this repository to
[gastownhall/bdp at 53bdbd03](https://github.com/gastownhall/bdp/tree/53bdbd03136875f952af184fce7b3c7af8f74e96).
It is an example consumer, not a complete protocol validator or SDK.

Start the graph BDP service separately using the repository's
[HTTP read instructions](../../docs/reference/graph-cli-specification-draft.md#bdp-read-from-scripts). Its configured
Scope must be reachable at the canonical URL. The preview serves an initialized
ordinary shared-server graph workspace; embedded HTTP serving is not supported.

From the repository root, enumerate **all Beads**:

```sh
python3 examples/bdp-read/read_beads.py \
  --scope http://127.0.0.1:8765/demo/ --limit 1
```

`--limit 1` deliberately requests one item per page. The script discovers
`bdp.json`, checks its Scope and Read profile, then follows each complete returned
`next` URL until it is `null`. It also follows a next URL on an empty page. The
result is one JSON array containing both Issue and Memory Beads. With no `--limit`,
the page size is 100. It never interprets the first page as the entire inventory.

Read one current Bead by its canonical identity:

```sh
python3 examples/bdp-read/read_beads.py \
  --scope http://127.0.0.1:8765/demo/ \
  --id http://127.0.0.1:8765/demo/beads/plan
```

This prints one Bead object. These are current reads, not History requests.
There is no CLI fallback if the HTTP server is unavailable.

For an authenticated service, have your shell or secret manager provide
`BDP_TOKEN` in the process environment. The script attaches it as a Bearer token
on every request, including discovery and continuation. It does not accept the
token on the command line or print it in diagnostics. Use HTTPS for credentials
across an untrusted network; the preview listener itself does not provide TLS.

The example refuses redirects and ambient HTTP proxies. Before attaching a
credential it checks the configured exact scheme/authority and Scope path;
continuations must additionally retain the exact Bead collection path. It rejects
relative, credential-bearing, fragment-bearing and unsafe encoded paths. This
conservative example does not resolve transport aliases or rename a Scope.

Malformed responses, HTTP errors, expired cursors, repeated pages and duplicate
Bead IDs exit nonzero. Output is withheld until the entire operation succeeds,
so a failed later page does not leave a plausible partial inventory on stdout.
There are local safety bounds of 8 MiB per response, 10,000 pages and 10,000 Beads,
and a 15-second timeout per HTTP operation. The service's preview bounds may be
smaller. A cursor lost on service restart requires starting enumeration again;
the script does not quietly retry against a different snapshot.

The offline tests exercise pagination, shape errors and credential boundaries:

```sh
python3 -m unittest discover -s examples/bdp-read -p 'test_*.py'
```

Those tests substitute HTTP responses. Delivery evidence must also run this
unchanged example against the actual installed CLI's BDP HTTP service. The tests
alone do not qualify a real-server round trip.

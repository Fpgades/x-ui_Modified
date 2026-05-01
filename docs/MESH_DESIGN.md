# xui-mesh: Master-Node Architecture Design

Status: DRAFT v0.1
Baseline: 3x-ui @ 51e2fb6 (mhsanaei/3x-ui master)
Author: design phase, not yet implemented

---

## 1. Goal

Add Marzban-style master/node clustering to 3x-ui without forking off into a
separate node binary. One bundle, three runtime modes selectable from the
panel: `standalone` (default, behaves exactly like upstream 3x-ui),
`master` (control plane), `node` (data plane managed by a master).

Drop-in replaceable for existing 3x-ui installs: when first launched on an
existing DB, panel comes up in `standalone` mode and nothing else changes
until the user explicitly switches mode.

## 2. Why this architecture

3x-ui already speaks gRPC to its local Xray over `127.0.0.1:apiPort`
(see `xray/api.go`: `HandlerServiceClient`, `StatsServiceClient`). The
master-node split therefore costs us only an extra gRPC layer between
the panel's "control" half and the panel's "Xray driver" half — both of
which already exist. No new dependencies (gRPC, protobuf, mTLS via stdlib
crypto/tls are all already on the import graph).

We deliberately do NOT use HTTP+JWT for the control channel: gRPC gives
us a typed contract (`.proto`), bidirectional streams (real-time logs and
heartbeats), and matches the existing in-process gRPC code paths.

## 3. Runtime modes

A single setting `panel_mode` in the `settings` table:

| Mode         | Web UI         | Local Xray | Nodes section | Pushes to remote nodes | Accepts master commands |
|--------------|----------------|------------|---------------|------------------------|-------------------------|
| `standalone` | full           | yes        | hidden        | no                     | no                      |
| `master`     | full + Nodes   | yes (self-node) | visible | yes                    | no                      |
| `node`       | locked, status | yes (driven by master) | hidden | no              | yes (mTLS gRPC, port N) |

**Key design decision (v1):** `master` mode also runs Xray locally — the
master IS itself a node from the data-plane perspective. This means a
single-server cluster works: install panel, switch to `master`, all
inbounds run locally, Nodes section is empty. Later, add a remote `node`
server to scale out — old inbounds keep running on master, new inbounds
can be assigned to either master or the new node.

Functionally `standalone` and `master` are very similar; the only
difference is whether the **Nodes** management UI is shown and whether
the panel maintains the gRPC client pool to remote nodes. We keep them
as distinct modes to give first-time users the simpler 3x-ui experience
by default.

Internally each panel has a synthetic `Node` row representing
"this machine" (id=1, name=`local`, address=auto-detected). Inbounds
created in `standalone` get `node_id=1`. Switching to `master` reuses
the same row; remote nodes get id=2, 3, … This way the
Inbound→Node link is always defined and `standalone↔master` mode
switching needs no data migration.

Mode transitions:
- `standalone ↔ master`: free, just toggles the Nodes UI and gRPC client
  pool startup. No data changes.
- `* → node`: requires zero remote nodes assigned and (for master)
  zero non-local inbounds. Wipes node identity, re-bootstraps for pairing.
- `node → standalone`: requires explicit "unpair" first (DB password
  re-entry). Then panel keeps the master-pushed config as local DB
  inbounds and behaves like standalone going forward.

## 4. Data model changes

New tables (additive, all migrations go through `database.initModels`):

### `nodes` (master only, but defined on all modes)
| field          | type    | notes                                              |
|----------------|---------|----------------------------------------------------|
| id             | int PK  |                                                    |
| name           | string  | unique, human-readable                             |
| address        | string  | hostname/IP that **clients** connect to (subscription) |
| api_address    | string  | hostname/IP that **master** connects to (control plane); often same as address but could be private VPN |
| api_port       | int     | gRPC port on the node                              |
| ca_cert_pem    | text    | node's CA cert pinned by master                    |
| client_cert    | text    | master's mTLS client cert (issued by master CA)    |
| client_key     | text    | (encrypted at rest? v1: stored plain in SQLite)    |
| status         | string  | `pending`, `connected`, `disconnected`, `error`    |
| last_seen      | int64   | unix ms                                            |
| version        | string  | reported by node                                   |
| xray_version   | string  | reported by node                                   |
| created_at     | int64   |                                                    |

### `node_identity` (node only)
Singleton row holding the node's own keypair, the master's pinned cert,
and the bootstrap token used during pairing.

### `inbounds` — new column
`node_id INTEGER NOT NULL DEFAULT 1`, foreign key to `nodes(id)`,
indexed. On upgrade from upstream 3x-ui: migration creates the synthetic
`local` node (id=1) first, then runs ALTER TABLE adding the column with
default 1. Existing inbounds therefore all get `node_id=1` and
behaviourally nothing changes.

For v2 we promote to a real M:N table (`node_inbounds`) so that one
logical inbound can fan out across many nodes with the same UUIDs
(Marzban's default UX).

## 5. Control-plane protocol (`mesh.proto`)

Lives at `mesh/proto/mesh.proto`, generated into `mesh/pb/`. Service:

```proto
service Mesh {
  // node->master pairing using a one-shot bootstrap token; returns
  // signed client cert + master CA cert. Called once per node.
  rpc Pair(PairRequest) returns (PairResponse);

  // master->node, push the full Xray config that this node should run.
  // Idempotent. Node atomically swaps config and reloads xray.
  rpc ApplyConfig(ApplyConfigRequest) returns (ApplyConfigResponse);

  // master->node, granular client ops (avoids full reload for
  // add/remove/update of a single client, mirroring xray HandlerService).
  rpc AddClient(ClientOp) returns (OpResult);
  rpc RemoveClient(ClientOp) returns (OpResult);

  // master->node, stats pull (mirror of xray StatsService).
  rpc GetStats(StatsRequest) returns (StatsResponse);

  // bidirectional heartbeat with version, load, last-applied config hash
  rpc Heartbeat(stream HeartbeatPing) returns (stream HeartbeatPong);

  // master->node, stream xray + panel logs
  rpc StreamLogs(LogRequest) returns (stream LogChunk);
}
```

Transport: TCP/TLS, mTLS required. Default port `62050` (configurable).
Cert pinning both ways: node pins master CA, master pins node leaf cert.

Pairing flow (one-time per node):
1. On node, operator switches to `node` mode → panel generates a node
   keypair + self-signed CA, displays a **bootstrap token** (random 32B,
   short TTL ~10min) and the node's `api_address:api_port`.
2. On master, operator opens "Add Node", enters address + token, hits
   Pair. Master generates client cert signed by its own master CA and
   calls `Pair(token, master_ca, master_client_cert_csr)` over plain
   TLS (server cert validated by token-derived fingerprint).
3. Node verifies the token, returns its CA cert and accepts the client
   cert as authorized. Both sides persist the pinning. Token is burned.
4. Subsequent calls use mTLS with the pinned certs.

## 6. Code layout

```
mesh/                      # NEW
  proto/mesh.proto
  pb/                      # generated
  pki/pki.go               # generate CA/cert/CSR helpers
  bootstrap.go             # token gen + verify
  client.go                # master-side gRPC client wrapper
  server.go                # node-side gRPC server impl
database/model/model.go    # +Node, +NodeInbound, +Inbound.NodeId
web/controller/node.go     # NEW: master HTTP API for node CRUD
web/service/node.go        # NEW
web/service/mode.go        # NEW: mode switch logic
web/service/inbound.go     # MODIFIED: route ops to node when NodeId != 0
sub/subService.go          # MODIFIED: resolveInboundAddress consults Node
web/html/xui/nodes.html    # NEW: master Nodes page
web/html/xui/node_status.html  # NEW: node mode status page
main.go                    # MODIFIED: dispatch based on panel_mode
```

## 7. Behaviour matrix at boot

```
ensure synthetic node row id=1 (`local`) exists; create on first boot
read panel_mode from DB (default: standalone)

if standalone:
  start xray locally driven by inbounds where node_id=1
  serve full panel UI; hide Nodes section

if master:
  start xray locally driven by inbounds where node_id=1  (master IS a node)
  serve full panel UI; show Nodes section
  for each remote node row (id>=2):
    open gRPC client, ApplyConfig with that node's inbound subset
    start heartbeat goroutine

if node:
  start gRPC server on api_port (mTLS)
  start xray locally — but config comes from last ApplyConfig from master,
    not from local DB
  panel UI: status-only, "managed by master <name>" screen
  allow login (you may need to unpair) but lock all CRUD endpoints
```

## 8. Subscription rendering changes

`sub/subService.go::resolveInboundAddress(inbound)` currently returns
`inbound.Listen` or sniffed address. Change to:

```go
if inbound.NodeId != 0 {
    node := nodeService.Get(inbound.NodeId)
    return node.Address    // public address for clients
}
return existing logic
```

This is the *only* mutation needed in subscription code: the rest of the
link generation (params, security, transport) is identical because
node-side Xray runs the same config we'd run locally.

## 9. UI changes

### Master: new "Nodes" sidebar entry
- list view: name, address, status badge, version, last_seen, # inbounds
- "Add Node" modal: name, public address, control address, api_port,
  paste bootstrap token → Pair
- per-node detail: live heartbeat, log tail, "force resync"

### Inbound add/edit form: new "Node" select
- options: "Master (local)" if self-node ON, else each registered node
- when changed on existing inbound: confirm → master removes from old
  node, applies on new node, updates DB

### Node mode lockdown screen
- big banner "This panel is in NODE mode, managed by <master>"
- show: master address, last sync, applied config hash, xray status
- "Unpair" button (requires DB-level password re-entry) → reverts to
  standalone with empty inbounds

### Form-vs-JSON cleanup (separate task, see §11)

## 10. Open questions / deferred to v2

- **Same-UUID-across-nodes** (Marzban default): in v1 each inbound
  lives on exactly one node. To replicate, user duplicates the inbound
  with the same UUIDs/clients on another node. v2 promotes inbounds
  to a true M:N relation with `node_inbounds` table.
- **Stat aggregation**: v1 master pulls stats per-node and stores
  per-(client,node). UI shows summed totals. Sub link shows summed
  traffic per client.
- **Failover/HA**: not in scope. One master, N nodes.

## 11. UI: replacing JSON textareas with form fields

Audit pass over `web/html/xui/form/protocol/*` and `form/stream/*`:

Already form-based: VLESS clients, VMess clients, Trojan clients, basic
TCP, basic WS, Reality (mostly), TLS basics.

Currently still JSON or partial:
- sniffing config (partial)
- mux settings (raw JSON in some flows)
- xray sockopt (TCP Fast Open, mark, tProxy, interface) — exposed as
  raw JSON in stream settings; we'll add proper checkbox/number fields
- HTTPUpgrade transport — needs full form
- mKCP/QUIC headers — needs form

This is a separate, parallel work stream — does not block the
master/node delivery.

## 12. Migration & compatibility

- `panel_mode` defaults to `standalone`; existing 3x-ui DBs upgrade in
  place with a single ALTER TABLE adding `inbounds.node_id` (nullable).
- New tables (`nodes`, `node_identity`) are created empty and unused
  until mode is changed.
- All new HTTP routes live under `/panel/api/mesh/...` so they don't
  collide with existing API surface.
- Sub URLs unchanged; behaviour identical in standalone mode.
- x-ui.sh CLI gets new commands: `x-ui mode`, `x-ui pair-token`.

## 13. Build & deploy

- Same Makefile/Dockerfile as upstream. Add `make proto` target that
  runs protoc against `mesh/proto/mesh.proto`. Generated files
  committed (not generated at build time, to keep build environment
  simple).
- `install.sh` unchanged for v1; mode is chosen post-install via UI
  or `x-ui mode <standalone|master|node>` on the CLI.
- New systemd port to open in firewall: 62050/tcp (node mode only).

## 14. Testing plan

- Unit: pairing handshake, cert pinning, config-hash idempotency.
- Integration (single-host): two binaries on one VM, one as master one
  as node on a non-default port. Add inbound on master, verify Xray
  config on node, fetch sub URL, confirm address is node's.
- E2E (real): user's two foreign servers, master on one, node on the
  other, real Reality inbound, real client (v2rayng) imports sub.

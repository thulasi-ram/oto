# Reaching an incident tool: the envelope, a mapping, or a bridge

oto draws Incidents over Cases and hands the response off to the tool your team already runs
([ADR 0052](../adr/0052-an-incident-is-drawn-over-cases-and-its-response-is-handed-off.md) §5). It
does that with one thing: the generic webhook and its `oto.notification.v1` envelope
([webhook.md](webhook.md)). **oto ships no PagerDuty, incident.io or other vendor provider, and it
hosts no bridge** ([ADR 0055](../adr/0055-an-incident-tool-integration-is-data-or-a-bridge-never-code-in-oto.md)
§§3, 5). Everything below is one of three shapes of that webhook.

---

## 1. Three ways to reach a tool

| Route | What oto sends | You write |
|---|---|---|
| **The plain envelope** | `oto.notification.v1`, exactly as [webhook.md §3](webhook.md#3-the-envelope) describes it. | Nothing in oto. The tool accepts arbitrary JSON and maps the fields itself (a generic "custom webhook" alert source, a log pipeline). |
| **A payload mapping** | One request per fact, in the shape the tool's API wants, rendered from the envelope by a document on the webhook **connection**. | A mapping — or start from oto's [catalog](../../mappings/README.md) (incident.io, PagerDuty) and import a copy. See [webhook.md §7](webhook.md#7-reaching-a-tool-that-wants-a-different-shape-the-payload-mapping). |
| **A bridge** | The plain envelope, to a small service **you** run. | The service. It runs outside oto, in any language, and talks to the tool however the tool needs. |

### How to choose

- **A mapping** when one request per fact, in a fixed shape, is enough: the tool has an "alerts" or
  "events" endpoint that takes a JSON body and a token, and you can say what each field is from the
  envelope with Liquid and nothing else.
- **A bridge** for anything more:
  - **OAuth**, or any token that has to be fetched, refreshed or exchanged — a mapping only carries a
    static credential or a sealed `{{ secrets.<name> }}`.
  - **Several calls per fact**: open an incident *and* post a note, look something up first, attach
    each Case as a separate item. A mapping sends exactly one request.
  - **Reading a response** beyond the incident's link and id. A mapping can only point oto at
    `external_url` / `external_id` in a 2xx body ([webhook.md §5](webhook.md#echoing-your-incident-back));
    everything else in the response is discarded.
  - **Any logic**: per-team routing tables, state you keep between facts, a decision about whether a
    fact matters to the tool at all. A mapping cannot decline a fact — every one of the 20 renders.

If you find yourself wanting to add a vendor provider to oto instead, that is the request ADR 0052 §5
and ADR 0055 §5 refuse: oto's process holds every org's channel credentials and its database, and
code for one vendor's API does not run there.

---

## 2. `quiet` is not `fixed`

ADR 0055 §4, in its own words: **"The docs say plainly that `quiet` is not `fixed`, and that a
mapping which resolves on `quiet` ends a response the moment the signals stop."**

`quiet` is an Incident fact that says **every current member Case has closed** — the envelope's
`incident.state` is "`active` while any current member Case is open and `quiet` otherwise. Derived by
oto from its Cases and never set by anyone"
([`envelope.go`](../../internal/channels/render/webhookjson/envelope.go)). It is a statement about
signals, not about the problem. All of these produce `quiet` with nothing fixed:

- a **flapping** alert that resolved between two evaluations;
- a **scrape or exporter outage** — the thing emitting the signal died, so the signal stopped;
- a **rule edit** that renamed or retired the alert while the problem carried on.

A mapping or a bridge that turns `quiet` into the tool's resolve, close or acknowledge **ends the
response at that moment**: the incident closes, the tool stops paging anyone about it, and nothing
pages when the signal comes back. When it does come back, oto sends **`active_again`** — to an incident
your tool has already closed. What the tool does with a new event for a closed incident (reopen it,
open a fresh one, or nothing) is the tool's behaviour, not oto's; either way the people working the
response were already told it was over.

oto's own code never sends that command, and no mapping in oto's catalog does — a test refuses one
([mappings/README.md](../../mappings/README.md)). You may write it yourself: it is then a rule you
wrote, keyed on a fact oto stated, and the cost above is yours to accept. Ending a response is
usually a human's call in the incident tool.

---

## 3. The bridge pattern

A bridge is a webhook receiver. Point a webhook channel at it, with **no payload mapping** on the
connection, and give the connection a **signing secret**. Then, for every request:

1. **Verify it came from oto.** Check `X-Oto-Timestamp` is within five minutes of your clock and that
   one `v1=` entry of `X-Oto-Signature` is `HMAC-SHA256(secret, "v1:" + timestamp + ":" + body)` over
   the raw bytes — [webhook.md §4](webhook.md#4-verify-that-a-request-came-from-oto), which has a
   worked example pinned by oto's own signing test. Refuse anything else.
2. **De-duplicate on `X-Oto-Delivery-Id`.** oto's delivery queue is **at-least-once**: a request your
   bridge has already handled can arrive again (a retry after a timeout, a worker restart).
   `X-Oto-Delivery-Id` is the same on every attempt of one delivery, so a bridge that remembers the
   ids it has finished answers a repeat with 2xx and does nothing twice.
3. **Key the external incident on `incident.id`.** It is stable for the Incident's whole life; every
   later fact about the same Incident carries it. Upsert on it, and later facts update one external
   incident instead of opening new ones. (`incident.number` is for humans.) Case facts carry no
   `incident`; a bridge that only cares about Incidents answers them 2xx and moves on.
4. **Answer 2xx only once the work is durable** — the tool has accepted the call and you have
   recorded the delivery id. oto decides what to do from your status code alone
   ([webhook.md §5](webhook.md#5-what-oto-does-with-your-response)): `429` is retried after
   `Retry-After`; `408` and `5xx` are retried with backoff, as are a timeout and a connection error;
   `401`/`403` stop and mark the channel's credential failed; any other `4xx` is permanent and never
   retried. So when the tool is down, answer `503` and let oto try again — a `400` drops the fact for
   good.
5. **Optionally, echo the incident back.** On an Incident fact, a 2xx JSON body with top-level
   `external_url` (absolute `https://`) and `external_id` makes oto link the tool's incident from the
   Incident's page and Slack card ([webhook.md §5](webhook.md#echoing-your-incident-back)). The first
   valid answer per Incident per channel is kept, so answer with the same incident every time.

### A minimal bridge

⚠️ **A sketch, not a product.** oto does not ship, support or run this; it shows the five steps
above in one place. `open_or_update` is yours — the vendor's REST calls, with its own credentials,
read from your environment.

```python
# Unsupported sketch: a bridge from oto's webhook to an incident tool's API.
import json, os, sqlite3
from http.server import BaseHTTPRequestHandler, HTTPServer
from verify import verify            # the function in webhook.md §4

SECRET = os.environb[b"OTO_SIGNING_SECRET"]
db = sqlite3.connect("bridge.db", isolation_level=None)
db.execute("CREATE TABLE IF NOT EXISTS done (delivery_id TEXT PRIMARY KEY)")
db.execute("CREATE TABLE IF NOT EXISTS incidents (oto_id TEXT PRIMARY KEY, url TEXT, id TEXT)")

class Bridge(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        if not verify(SECRET, self.headers, body):                       # 1
            return self.answer(401)
        delivery = self.headers["X-Oto-Delivery-Id"]
        if db.execute("SELECT 1 FROM done WHERE delivery_id = ?", (delivery,)).fetchone():
            return self.answer(200)                                      # 2
        env = json.loads(body)
        echo = {}
        if "incident" in env:                                            # 3
            try:
                url, ext_id = open_or_update(env["incident"]["id"], env)  # never resolves on quiet
            except Exception:
                return self.answer(503)                                  # 4: oto retries
            db.execute("INSERT OR REPLACE INTO incidents VALUES (?, ?, ?)",
                       (env["incident"]["id"], url, ext_id))
            echo = {"external_url": url, "external_id": ext_id}          # 5
        db.execute("INSERT INTO done VALUES (?)", (delivery,))
        self.answer(200, echo)

    def answer(self, status, payload=None):
        out = json.dumps(payload or {}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

HTTPServer(("", 8080), Bridge).serve_forever()
```

Two things the sketch leaves to you. If the bridge dies between the tool's call succeeding and the
delivery id being recorded, the retry calls the tool again — pass the delivery id as the tool's own
idempotency key where it has one. And `open_or_update` decides what each fact means to the tool;
`quiet` is in that list, and §2 is what it costs to read it as "resolved".

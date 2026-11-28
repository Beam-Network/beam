---
id: media-roles
title: Media Roles
sidebar_label: Media Roles
sidebar_position: 6
---

# Media Roles

Workers can serve Beam media: calls, stages and livestreams for applications
built on Beam, and Beam's own CDN for broadcasts. Media work is opt-in per
**role**: an operator enables the roles a machine can serve well, and Beam places
media nodes on it according to its measured capacity, region and audit history.

---

## The roles

| Role | What it does | Needs | Sees content |
| --- | --- | --- | --- |
| `media.sfu` | The origin of a call or stage: receives every publisher over WebRTC (WHIP) and forwards to each subscriber (WHEP), adapting layers per subscriber | Public IP, open UDP range, CPU, 1 Gbit/s or more | No for end-to-end encrypted sessions; yes for public ones |
| `media.relay` | Re-serves a stage's tracks to listeners in its region, pulling from the SFU or another relay | Public IP, open UDP range, 1 Gbit/s or more | As the SFU |
| `media.ingest` | Receives a broadcast from an encoder over WHIP, RTMP(S) or SRT | Public IP, TCP 443, 1935, 1936 and UDP 9000 open | Yes |
| `media.transcode` | Encodes the source into the session's renditions, or for a source-only broadcast copies the publisher's H.264 as one rendition and encodes only the audio | NVIDIA GPU with NVENC (CPU for audio and low renditions); none for source-only broadcasts | Yes |
| `media.package` | Packages renditions as LL-HLS, signs segment hashes, writes recordings to the Worker's bucket | CPU, disk, bucket access | Yes, or ciphertext output in protected mode |
| `media.shield` | Regional cache between the packager and the edges | Public IP, TCP 443, 1 Gbit/s or more, cache disk | Public or ciphertext |
| `media.edge` | Serves HLS to players, caching in memory and on disk | Public IP, TCP 443, 1 Gbit/s or more, cache disk | Public or ciphertext |

A Worker can enable `media.transcode` **without a GPU**. It then reports no
NVENC session (`gpu: null` in its capacity report) and Beam gives it only
source-only broadcasts (`ladder: "source"`): the video is copied without
decoding and only the audio is encoded to AAC on the CPU, so a few CPU cores
serve such a broadcast. Broadcasts with an encoded ladder still go only to
Workers with free NVENC sessions. A GPU Worker takes both kinds.

Every media role needs a **public IP address** today: Workers behind home NAT
cannot take media roles yet. Transcode and package nodes go only to Workers with
a strong audit history.

Each media node reserves Worker memory: 128 MiB for an SFU, relay or ingest
node, 256 MiB for a packager, shield or edge node, and 512 MiB for a
transcoder. Raise `--memory-bytes` to fit the nodes a Worker holds at once.

`media.transcode` runs ffmpeg in a sandbox container with Docker, and with the
NVIDIA container runtime on GPU Workers. Build the transcoder image once from
the root of the `beam` repository:

```text
docker build -f actors/worker/media-transcoder.Dockerfile -t media-transcoder:latest .
```

---

## Enable a role

Media roles use the `--media-*` flags of `beam-worker` (or their `BEAM_MEDIA_*`
environment variables). An edge on a fresh VPS:

```text
./bin/beam-worker serve --node-key data/worker/node.key \
  --media-roles media.edge,media.shield \
  --media-region weur \
  --media-egress-mbps 1000 --media-ingress-mbps 1000 \
  --media-self-test-url https://speed.cloudflare.com \
  --media-probe-trusted-cidrs "$BEAM_MEDIA_PROBE_TRUSTED_CIDRS" \
  --media-public-ip 203.0.113.7 \
  --media-https-addr :443 \
  --media-cache-dir /var/lib/beam/media-cache \
  --media-cache-bytes 200000000000 --media-cache-memory-bytes 8000000000
```

| Flag | Environment | Meaning | Default |
| --- | --- | --- | --- |
| `--media-roles` | `BEAM_MEDIA_ROLES` | Comma list of roles: `media.sfu`, `media.relay`, `media.ingest`, `media.transcode`, `media.package`, `media.shield`, `media.edge` | none (no media role) |
| `--media-region` | `BEAM_MEDIA_REGION` | The broad region you claim: `wnam`, `enam`, `sam`, `weur`, `eeur`, `apac`, `oc`, `afr`, `me` | none |
| `--media-egress-mbps`, `--media-ingress-mbps` | `BEAM_MEDIA_EGRESS_MBPS`, `BEAM_MEDIA_INGRESS_MBPS` | Bandwidth you declare | none |
| `--media-self-test` | `BEAM_MEDIA_SELF_TEST` | Measure bandwidth at start and every 6 hours, with the server of `--media-self-test-url` | true |
| `--media-self-test-url` | `BEAM_MEDIA_SELF_TEST_URL` | Speed test server of the self test, with the Cloudflare speed test API (`POST /__up`, `GET /__down?bytes=N`): `https://speed.cloudflare.com` or your own | none: no self test, only the declared bandwidth is reported (logged once) |
| `--media-probe-trusted-cidrs` | `BEAM_MEDIA_PROBE_TRUSTED_CIDRS` | Comma list of CIDRs or addresses of Beam's bandwidth probe sources, as the network publishes them; their probes keep a reserved slot and budget | none |
| `--media-edge-domain` | `BEAM_MEDIA_EDGE_DOMAIN` | Override of the edge domain, for a local or development network only. Workers learn the domain from Beam (below) | none: the domain Beam sends |
| `--media-https-addr` | `BEAM_MEDIA_HTTPS_ADDR` | Browser-facing HTTPS listener | `:443` |
| `--media-rtmp-port`, `--media-rtmps-port`, `--media-srt-port` | `BEAM_MEDIA_RTMP_PORT`, `BEAM_MEDIA_RTMPS_PORT`, `BEAM_MEDIA_SRT_PORT` | Ingest ports; encoders expect the defaults | 1935, 1936, 9000 |
| `--media-cache-dir`, `--media-cache-bytes`, `--media-cache-memory-bytes` | `BEAM_MEDIA_CACHE_DIR`, `BEAM_MEDIA_CACHE_BYTES`, `BEAM_MEDIA_CACHE_MEMORY_BYTES` | Edge and shield cache | none |
| `--media-gpu-encoder-sessions` | `BEAM_MEDIA_GPU_ENCODER_SESSIONS` | NVENC sessions offered (0 or no GPU: source-only transcodes) | detected |
| `--media-public-ip`, `--media-udp-port-min`, `--media-udp-port-max` | `BEAM_MEDIA_PUBLIC_IP`, `BEAM_MEDIA_UDP_PORT_MIN`, `BEAM_MEDIA_UDP_PORT_MAX` | Public address and WebRTC UDP range | as for Room media |
| `--media-ice-servers`, `--media-turn-secret`, `--media-turn-credential-ttl`, `--media-turn-username-prefix` | `BEAM_MEDIA_ICE_SERVERS`, `BEAM_MEDIA_TURN_SECRET`, … | Your own STUN/TURN (coturn REST secret) for clients behind strict networks | none |

`media.package`, `media.shield` and `media.edge` need `--media-cache-dir`; the
packager keeps its DVR window there.

The Worker advertises the matching capability for each role
(`room.media.sfu.v2`, `room.media.relay.v1`, `room.media.ingest.v1`,
`room.media.transcode.v1`, `room.media.package.v1`, `room.media.shield.v1`,
`room.media.edge.v1`). Update the orchestrator and its Workers together.

### Ports

| Role | Open inbound |
| --- | --- |
| sfu, relay | TCP 443 (WHIP, WHEP, ICE), the UDP range |
| ingest | TCP 443 (WHIP), TCP 1935 (RTMP), TCP 1936 (RTMPS), UDP 9000 (SRT) |
| shield, edge, package | TCP 443 |
| transcode | none beyond the Worker's usual outbound access |

Traffic must reach the Worker directly: no TLS-terminating proxy or load
balancer in front of the media ports.

### Certificates and host names

You do not buy or configure a certificate, and you do not choose the domain.
Each browser-facing Worker gets a host name `<label>.edge.<edge domain>` (the
label is the Worker id when it is a plain DNS label, else a hash of it). The
edge domain belongs to the network: Beam sends it to the Worker through its
orchestrator, in the acknowledgement of each media capacity report
(`edge_domain`), so a Worker needs no setting for it and follows a change of
domain on the next report. `--media-edge-domain` only overrides it on a local or
development network. The Worker generates its own key, sends Beam a certificate
request for that one name, and Beam issues the certificate through ACME and
publishes the DNS record. Ingest Workers also receive the certificate of each
session's stable ingest name while they hold it. Certificates are renewed
automatically and stop being renewed when a Worker leaves. The Worker never
holds a wildcard certificate, and it must not set cookies.

---

## Capacity, placement and checks

Your orchestrator sends Beam a capacity report for each media Worker every 30
seconds: roles, region claim, declared and measured bandwidth, UDP range,
ports, GPU encoder sessions, cache size and live load. Beam does not take those
figures on trust:

- **Probes** measure bandwidth and check the region claim by round-trip times
  from several locations. Placement uses the verified values, with headroom
  (about 80 % of the verified bandwidth): an inflated declaration earns nothing.
- **Auditors** join sessions like ordinary viewers and check availability,
  latency, segment hashes, relay continuity and sampled frames. A Worker that
  fails audits loses placement for the roles concerned.
- A Worker missing from reports for 90 seconds is treated as having no media
  capacity; its nodes move to other Workers and viewers are moved without
  action on their side.

### Bandwidth probe

Beam verifies the egress you declare by downloading from the Worker:
`GET /v1/probe/bandwidth?bytes=N` on the media HTTPS port
(`--media-https-addr`), by the Worker's public IP and without credentials. The
answer is `N` filler bytes, about 25 MB every 6 hours. Because the port is
public, the Worker bounds the endpoint:

- `N` is at most 100,000,000 (larger or missing: 400);
- three probes at once, one of them kept for Beam's probe sources;
- at most 2 GB of probe data per hour, 500 MB of it kept for Beam's probe
  sources;
- any other network (a /24 for IPv4, a /64 for IPv6): one probe at a time, one
  per minute, 200 MB per hour;
- a Beam probe source: one probe per 10 seconds.

Beyond a bound the answer is 429 with `Retry-After`; a client that closes early
is refunded the bytes not sent. Set `--media-probe-trusted-cidrs` to the probe
sources the network publishes, so their probes keep their slot when others use
the endpoint. Keep the port reachable from the internet: a probe that fails
leaves the Worker's egress unverified.

### Drain a Worker

Before maintenance, ask your orchestrator to drain the Worker:

```text
curl -X POST http://127.0.0.1:8781/v1/orchestrator/media/drain \
  -H "Authorization: Bearer $(cat data/orchestrator/control-token)" \
  -H 'Content-Type: application/json' \
  -d '{"worker_id":"<worker id>","drain":true}'
```

A draining Worker keeps serving its nodes and gets no new one: the
orchestrator reports it to Beam with no media role (`roles: []`). Beam places
replacement nodes on other Workers and moves clients to them without a
reconnect, then stops the old nodes. The drain is done when
`GET /v1/orchestrator/media/nodes` lists no node on the Worker;
`"drain": false` puts it back. To leave media roles for good, drain, then
restart the Worker without `--media-roles`.

---

## What is rewarded

Media work earns PRISM like other work, from **verified** delivery only. A
node's signed usage reports are cross-checked against the viewers' quality
beacons and Beam's audits; usage that does not match is excluded and the Worker
flagged.

The inputs, per role:

- **Delivered minutes and bytes** to real viewers and participants (relays,
  edges, SFUs, shields, ingest);
- **Quality of service**: latency, loss, rebuffering and uptime, from beacons
  and audits;
- **Transcode rendition-minutes**, counting the renditions the node encodes
  (on NVENC for video, on the CPU for audio): a source-only broadcast counts
  its audio rendition only, since its video is copied;
- **Penalties** for dropped sessions and failed audits.

Customers may be billed per minute, per GB or per peak of the same session, and
a price may depend on the class of the media (`audio`, `sd`, `hd`, `fhd`,
`uhd`, or `idle` for a connection without media), on whether the participant is
a person or a bot, and on the transport. The GB of a customer are the media
bytes your nodes exchanged with its clients: retransmissions, padding and the
traffic between nodes are never billed. Media work's share of emission is its
share of the list-price credits billed for counted work; see [Weights](./weights).

Each role has its own weight, and quality weighs on the delivered volume. Weight
values are published separately and may change; see [PRISM Scoring](./prism)
for how scores turn into weights.

---

## Related Pages

- [Workers](./workers)
- [Orchestrators](./orchestrators)
- [Rooms](./rooms)
- [PRISM Scoring](./prism)

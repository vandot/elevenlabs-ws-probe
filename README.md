# elevenlabs-ws-probe

A minimal client for characterising how ElevenLabs' streaming text-to-speech
WebSockets behave: when the server gives up on an idle connection, what it sends
when it does, how it reports failures, and how the two streaming endpoints
differ from each other.

No agent framework, no SDK, one WebSocket library. That is the point — if a
small client reproduces a behaviour, the behaviour belongs to the server rather
than to a stack sitting on top of it.

## Why

Voice agents hold sockets open across long silences, and the documentation
defines `inactivity_timeout` without saying what the server actually does when
it fires. Reports against [livekit/agents](https://github.com/livekit/agents)
describe ElevenLabs WebSockets closing or going quiet mid-session with the cause
unclear ([#4609](https://github.com/livekit/agents/issues/4609),
[#4676](https://github.com/livekit/agents/issues/4676)).

This tool measures the behaviour directly.

## Install

```sh
go build -o elevenlabs-ws-probe .
export ELEVENLABS_API_KEY=...   # a key scoped to Text to Speech
```

**Pick a voice your account can actually use.** Library voices are not available
to free accounts over the API, and the way that failure surfaces differs by
endpoint (see finding 4). The default is George, a standard voice that works on
free accounts.

## Usage

```sh
./elevenlabs-ws-probe -scenario idle                        # single-stream
./elevenlabs-ws-probe -scenario idle -mode multi            # multi-stream-input
./elevenlabs-ws-probe -scenario speak -mode multi -max 30s
```

Every event is printed with elapsed time since start, and written as JSONL with
`-out` so traces can be compared or charted.

### Scenarios

| Scenario | What it does |
|---|---|
| `idle` | Connect, send the initialisation message, then nothing. Measures when and how the server reacts. |
| `keepalive` | As `idle`, but send a single space every `-keepalive`. |
| `empty` | Send `{"text":""}` after a short pause. The two endpoints define opposite meanings for this payload. |
| `speak` | Synthesise `-text`, log audio chunks and `isFinal`, then idle. |
| `revive` | Wait for the context to be finalised, then write to that same context. |
| `newctx` | Wait for the context to be finalised, then open a second context on the same socket. |

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-mode` | `single` | `single` (`/stream-input`) or `multi` (`/multi-stream-input`) |
| `-model` | `eleven_turbo_v2_5` | model id |
| `-voice` | `JBFqnCBsd6RMkjVDRZzb` | voice id (George) |
| `-language` | — | `language_code` query parameter |
| `-inactivity-timeout` | `0` | seconds; `0` omits it, server default 20 |
| `-keepalive` | `15s` | keepalive interval for `scenario=keepalive` |
| `-max` | `5m` | how long to wait before giving up |
| `-out` | — | append a JSONL trace to this file |

## Confirmed as documented

Included because a reproduction is only useful if it also reports the parts that
behave as described.

- A single space `" "` keeps a connection alive. A space every 15s held a
  multi-stream context open past 90s with no timeout.
- `{"text":""}` on `/stream-input` ends the sequence and closes the socket:
  close code 1000, roughly 0.9s after the message.
- `{"text":"", "context_id": ...}` on `/multi-stream-input` is `KeepContextAlive`
  and resets the timer. The context finalised 20.1s after the empty message
  rather than 20s after connecting.

## Findings

Measured October 2026, `eleven_turbo_v2_5`, from a residential connection in
Slovenia. Every audio-dependent result was checked against a control run on a
fresh connection.

### 1. The two endpoints handle an idle timeout differently

| | `/stream-input` | `/multi-stream-input` |
|---|---|---|
| At ~20s | JSON message, then **close 1008** | `isFinal` on the context |
| Socket afterwards | closed at 21.3s | **still open at 5–6 minutes** |
| Reason given | yes, in the close frame | none |

`/stream-input`:

```
20704ms  recv   {"message":"Have not received a new text input within the timeout of 20 seconds. Streaming input terminated..."}
21271ms  close  [code 1008]
```

`/multi-stream-input`:

```
20499ms  recv   isFinal (context probe-ctx-1)
300357ms done   socket still open at -max=5m0s
```

The reference documents `inactivity_timeout` for both endpoints without stating
that one closes the transport and the other only ends the context. The endpoints
differ by a single path segment, so a client written against one will misread
the other. No close codes are documented for either.

### 2. A finalised context can be revived

Also undocumented, and useful: after a context has been finalised by the idle
timeout, writing to that same `context_id` resumes generation.

```
20371ms  recv   isFinal (context probe-ctx-1)
22386ms  send   text after final
22821ms  recv   audio chunk (43,093 bytes)
```

Opening a second context on the same socket works as well. A multi-stream socket
therefore survives an idle timeout intact — nothing needs to be torn down and
re-established.

### 3. `inactivity_timeout` above the maximum is silently clamped

The parameter is documented with a maximum of 180 seconds, with nothing said
about larger values. Passing `inactivity_timeout=300` produced no error and no
warning, and the context finalised at 180.3s:

```
    229ms  send     init (BOS)
 180328ms  recv     isFinal (context probe-ctx-1)
 360235ms  done     socket still open at -max=6m0s
```

Anyone setting a value above the cap is reasoning about a window they do not
have.

### 4. An unauthorised voice produces no error on multi-stream

Free accounts cannot use library voices over the API. The same key, voice and
request shape reports this very differently depending on the endpoint.

`/stream-input` — explicit:

```
533ms  recv  {"message":"Free users cannot use library voices via the API. Please upgrade your subscription to use this voice.", ...}
```

`/multi-stream-input` — silent:

```
240ms  send  init (BOS)
240ms  send  text
322ms  recv  isFinal (context probe-ctx-1)     ← no audio, no error
```

On multi-stream the connection is healthy, the write succeeds, a response
arrives, and there is simply no audio. Nothing distinguishes it from a
successful request that happened to produce nothing. The same request on the
sibling endpoint explains the problem in one line.

This is the failure mode most likely to cost someone debugging time, and it is
the one a voice agent would present as "it stopped talking, and the logs look
fine".

## Reproducing

Everything behind the findings, in one go:

```sh
ELEVENLABS_API_KEY=... ./run-all.sh [voice_id]
```

That writes one trace per scenario into a timestamped directory and takes about
fifteen minutes, nearly all of it waiting for timeouts to fire.

Individually:

```sh
./elevenlabs-ws-probe -scenario idle                            -out out-idle.jsonl
./elevenlabs-ws-probe -scenario idle    -mode multi             -out out-multi.jsonl
./elevenlabs-ws-probe -scenario idle    -mode multi -language tr \
                      -inactivity-timeout 300 -max 6m           -out out-clamp.jsonl
./elevenlabs-ws-probe -scenario speak   -mode multi -max 30s    -out out-speak.jsonl
./elevenlabs-ws-probe -scenario revive  -mode multi -max 90s    -out out-revive.jsonl
./elevenlabs-ws-probe -scenario newctx  -mode multi -max 90s    -out out-newctx.jsonl
./elevenlabs-ws-probe -scenario keepalive -mode multi -max 90s  -out out-keepalive.jsonl
./elevenlabs-ws-probe -scenario empty                 -max 60s  -out out-empty-single.jsonl
./elevenlabs-ws-probe -scenario empty   -mode multi   -max 60s  -out out-empty-multi.jsonl
```

To see finding 4, run `speak` with a library voice such as
`-voice 21m00Tcm4TlvDq8ikWAM` on a free account, once against each endpoint.

All traces cited above are published under [`traces/`](traces/), one file per
run, with an index describing what each shows.

## Caveats

One account, one region, one voice, one model, measured on a single day. Timeout
values are server behaviour and may change. The value of the tool is that anyone
can re-measure rather than take these numbers on trust.

The probe mostly connects and waits, so it generates almost no load and almost
no cost.

## Licence

MIT

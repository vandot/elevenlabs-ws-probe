# Traces

Raw JSONL output from the runs cited in the main README. One event per line,
each with elapsed time since the probe started.

| File | Shows |
|---|---|
| `01-idle-single.jsonl` | `/stream-input` idle: error message, then close 1008 at 21.3s |
| `02-idle-multi.jsonl` | `/multi-stream-input` idle: context finalised at 20.5s, socket open at 5m |
| `03-clamp-300-to-180.jsonl` | `inactivity_timeout=300` honoured as 180s |
| `04-unauthorised-voice-multi.jsonl` | Library voice on multi-stream: `isFinal`, no audio, no error |
| `05-speak-valid-voice-multi.jsonl` | Same request with a permitted voice: audio at 1.1s |
| `06-unauthorised-voice-single.jsonl` | Same library voice on single-stream: explicit error message |
| `07-revive-context.jsonl` | Writing to a finalised context resumes generation |
| `08-second-context.jsonl` | A second context opens on the same socket after a timeout |
| `09-keepalive-multi.jsonl` | A space every 15s holds the context open past 90s |
| `10-empty-text-single.jsonl` | `{"text":""}` closes single-stream with 1000 |
| `11-empty-text-multi.jsonl` | `{"text":"", "context_id":...}` resets the multi-stream timer |

Files 04 and 05 come from the same original capture, split at the connection
boundary; 04 used a library voice the account could not access, 05 a permitted
one. Together they are the evidence for finding 4.

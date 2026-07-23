### stream-doctor report

**Target:** `rtsp://REDACTED@cam.example:554/Preview_01_main`
**Result:** capture OK
**Tool:** go-audio-stream/stream-doctor 0.1.0 (linux/amd64)

**Handshake**

| Step | Status | Time |
| --- | --- | --- |
| DIAL | ok | 12ms |
| DESCRIBE | ok | 8ms |
| SETUP | ok | 6ms |
| PLAY | ok | 7ms |

- Auth: Digest
- Session timeout: 60s
- Keepalive: GET_PARAMETER
- Transport: TCP interleaved, channels 0-1

**Tracks**

| # | Kind | Codec | Clock | Ch | Depacketize |
| --- | --- | --- | --- | --- | --- |
| 0 | audio | AAC | 16000 | 1 | yes |
| 1 | video | H264/90000 | 90000 | - | no |

**Capture (10s, track 0, ended: completed)**

| Metric | Value |
| --- | --- |
| Packets | 500 |
| Bytes | 64000 |
| Lost | 0 (0.00%) |
| Max gap | 0 |
| Bitrate | 51.2 kbit/s |
| Jitter | 0.59 ms |

**Listen:** wrote 10.0s of 16000 Hz mono s16 PCM

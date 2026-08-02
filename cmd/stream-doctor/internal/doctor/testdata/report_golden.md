```
stream-doctor 0.1.0 (linux/amd64)
target: rtsp://[redacted]/Preview_01_main
result: capture OK

handshake
  DIAL      ok    12ms
  DESCRIBE  ok    8ms
  SETUP     ok    6ms
  PLAY      ok    7ms

session
  server: TestCam/1.0
  auth: Digest
  session-timeout: 60s
  keepalive: GET_PARAMETER
  transport: TCP interleaved, channels 0-1

tracks
  track 0: audio, AAC, PT 97, clock 16000, ch 1, depacketize yes
    fmtp: mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1408
    asc: 1408
  track 1: video, H264/90000, PT 96, clock 90000, ch -, depacketize no

capture: track 0, window 10s, ended completed
  packets: 500
  received: 500
  bytes: 64000
  lost: 0 (0.00%)
  duplicates: 0
  malformed: 2
  ssrc-resets: 1
  max-gap: 0
  bitrate: 51.2 kbit/s
  jitter: 0.59 ms

listen: wrote 10.0s of 16000 Hz mono s16 PCM
```
